package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/controller"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/keycloak"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/probe"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/profilesync"
)

// runSync is one scheduled profile sync (the CronJob's command): read each
// linked user's record from each tier's directory and write the mapped
// attributes into the broker, the chain's earlier tiers winning. It records
// the outcome in status.sync and exits non-zero if any user failed.
func runSync(args []string) int {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	instance := fs.String("instance", "", "namespace/name of the IdentityContinuity")
	caFile := fs.String("ca-file", "", "extra CA bundle to trust (system roots always apply)")
	testTier := fs.String("test-tier", "", "only test this tier's directory (token, then a read of its users); status is not changed")
	_ = fs.Parse(args)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ns, name, ok := strings.Cut(*instance, "/")
	if !ok || ns == "" || name == "" {
		log.Error("need --instance namespace/name")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctrl.SetupSignalHandler(), 55*time.Minute)
	defer cancel()

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		log.Error("kube client", "err", err.Error())
		return 1
	}
	var ic v1.IdentityContinuity
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ic); err != nil {
		log.Error("identitycontinuity", "err", err.Error())
		return 1
	}
	if *testTier != "" {
		return testDirectory(ctx, c, &ic, *testTier, *caFile, log)
	}
	orig := ic.DeepCopy()
	started := metav1.Now()
	res, err := syncOnce(ctx, c, &ic, *caFile, log)
	st := ic.Status.Sync
	if st == nil {
		st = &v1.SyncStatus{CronJob: controller.SyncJobName(&ic)}
	}
	st.LastRun = &started
	st.Users, st.Updated, st.Failed = int32(res.Users), int32(res.Updated), int32(res.Failed)
	switch {
	case err != nil:
		st.Message = err.Error()
	case res.Failed > 0 || len(res.Errors) > 0:
		st.Message = summarize(res.Errors)
	default:
		st.LastSuccess, st.Message = &started, fmt.Sprintf("%d users read, %d updated", res.Users, res.Updated)
	}
	ic.Status.Sync = st
	if perr := c.Status().Patch(ctx, &ic, client.MergeFrom(orig)); perr != nil {
		log.Error("status", "err", perr.Error())
	}
	log.Info("sync finished", "users", res.Users, "updated", res.Updated, "failed", res.Failed, "message", st.Message)
	if err != nil || res.Failed > 0 || len(res.Errors) > 0 {
		return 1
	}
	return 0
}

func syncOnce(ctx context.Context, c client.Client, ic *v1.IdentityContinuity, caFile string, log *slog.Logger) (profilesync.Result, error) {
	if ic.Spec.Sync == nil {
		return profilesync.Result{}, errors.New("spec.sync is not set")
	}
	ref := ic.Spec.Sync.CredentialsRef.Name
	if ref == "" {
		ref = "continuity-sync"
	}
	id, secret, err := readCredentials(ctx, c, ic.Namespace, ref, "")
	if err != nil {
		return profilesync.Result{}, fmt.Errorf("broker credentials: %w", err)
	}
	b := ic.Spec.Broker.Keycloak
	kc := keycloak.New(b.URL, b.Realm)
	kc.SetCredentials(id, secret)
	prober, err := probe.New(caFile)
	if err != nil {
		return profilesync.Result{}, err
	}
	hc := prober.HTTPClient(20 * time.Second)

	var ts []profilesync.Tier
	for _, t := range ic.Spec.Tiers {
		if t.Type != "oidc" || t.OIDC == nil || t.Directory == nil || len(t.Claims) == 0 {
			continue
		}
		dir, err := directory(ctx, c, ic.Namespace, t, prober, hc)
		if err != nil {
			// The tier's attributes are kept as they are this run: a lower
			// tier never fills them because this one couldn't be read.
			log.Warn("directory unavailable", "tier", t.Name, "err", err.Error())
			dir = failed{err}
		}
		ts = append(ts, profilesync.Tier{Name: t.Name, Claims: t.Claims, Dir: dir})
	}
	if len(ts) == 0 {
		return profilesync.Result{}, errors.New("no tier has both a directory and claim mappings")
	}
	return profilesync.Run(ctx, kc, ts, controller.Writable(ic), func(msg string, kv ...any) { log.Info(msg, kv...) }), nil
}

// testDirectory checks one tier's directory along the sync's own path and
// credentials. The result (no user data) is the container's termination
// message, for whoever started the test.
func testDirectory(ctx context.Context, c client.Client, ic *v1.IdentityContinuity, tier, caFile string, log *slog.Logger) int {
	res := struct {
		OK      bool   `json:"ok"`
		Users   int    `json:"users"`
		Message string `json:"message"`
	}{}
	err := func() error {
		i := slices.IndexFunc(ic.Spec.Tiers, func(t v1.Tier) bool { return t.Name == tier })
		if i < 0 || ic.Spec.Tiers[i].Type != "oidc" || ic.Spec.Tiers[i].Directory == nil {
			return fmt.Errorf("tier %s has no directory", tier)
		}
		prober, err := probe.New(caFile)
		if err != nil {
			return err
		}
		d, err := directory(ctx, c, ic.Namespace, ic.Spec.Tiers[i], prober, prober.HTTPClient(20*time.Second))
		if err != nil {
			return err
		}
		ck, ok := d.(profilesync.Checker)
		if !ok {
			return errors.New("this directory type can't be tested")
		}
		res.Users, err = ck.Check(ctx)
		return err
	}()
	if err != nil {
		res.Message = err.Error()
	} else {
		res.OK, res.Message = true, fmt.Sprintf("connected: token issued, %d users readable", res.Users)
	}
	b, _ := json.Marshal(res)
	_ = os.WriteFile("/dev/termination-log", b, 0o644)
	log.Info("directory test", "tier", tier, "ok", res.OK, "users", res.Users, "message", res.Message)
	if !res.OK {
		return 1
	}
	return 0
}

// directory sets up the tier's directory: its credentials, and the token
// endpoint from the tier's discovery.
func directory(ctx context.Context, c client.Client, ns string, t v1.Tier, p *probe.Prober, hc *http.Client) (profilesync.Directory, error) {
	d := t.Directory
	id, secret, err := readCredentials(ctx, c, ns, d.CredentialsRef.Name, d.ClientID)
	if err != nil {
		return nil, err
	}
	res, disc := p.OIDC(ctx, t.OIDC.Issuer, 10*time.Second)
	if disc == nil {
		return nil, fmt.Errorf("discovery: %s: %s", res.Kind, res.Message)
	}
	return profilesync.New(d.Type, d.URL, profilesync.Credentials{TokenURL: disc.TokenEndpoint, ClientID: id, ClientSecret: secret,
		Scopes: d.Scopes, Audience: d.Audience}, hc)
}

// readCredentials reads client-id (unless given) and client-secret from a Secret.
func readCredentials(ctx context.Context, c client.Client, ns, name, id string) (string, string, error) {
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &s); err != nil {
		return "", "", err
	}
	if id == "" {
		id = string(s.Data["client-id"])
	}
	secret := string(s.Data["client-secret"])
	if id == "" || secret == "" {
		return "", "", fmt.Errorf("secret %s needs client-secret and a client id", name)
	}
	return id, secret, nil
}

type failed struct{ err error }

func (f failed) User(context.Context, string) (map[string]any, error) { return nil, f.err }

// summarize keeps status short: the first few errors and a count.
func summarize(errs []string) string {
	const keep = 3
	if len(errs) <= keep {
		return strings.Join(errs, "; ")
	}
	return fmt.Sprintf("%s; and %d more", strings.Join(errs[:keep], "; "), len(errs)-keep)
}
