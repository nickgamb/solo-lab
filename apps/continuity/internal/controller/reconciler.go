// Package controller reconciles IdentityContinuity: probe every tier, pick the
// active one, and make Keycloak (and the egress path) match. Level-triggered:
// every pass rebuilds Keycloak's side from the spec, so a Keycloak restart that
// re-imports the realm is repaired on the next pass.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/keycloak"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/probe"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/tiers"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/trust"
)

const (
	finalizer     = "continuity.lab.solo.io/cleanup"
	LabelTier     = "continuity.lab.solo.io/tier"
	LabelInstance = "continuity.lab.solo.io/instance"
	cfgInstance   = "continuity.lab.solo.io/instance" // IdP config key: who owns it
	cfgHash       = "continuity.lab.solo.io/hash"
	// AnnotationCheckTrust: a new value runs the trust checks now (the
	// Observatory's "Check now").
	AnnotationCheckTrust = "continuity.lab.solo.io/check-trust"
	// trustEvery: how often the trust checks run without a change. Slow on
	// purpose: the callback check is a sign-in attempt the IdP may log.
	trustEvery = 10 * time.Minute
	maxHistory = 20
	// cleanupGrace: how long deletion waits for Keycloak before giving up on
	// cleaning it (so a gone broker never wedges namespace deletion).
	cleanupGrace = 5 * time.Minute
	// maxMessage bounds a condition's message.
	maxMessage = 4096
)

var (
	serviceEntryGVK = schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1", Kind: "ServiceEntry"}
	authzPolicyGVK  = schema.GroupVersionKind{Group: "security.istio.io", Version: "v1", Kind: "AuthorizationPolicy"}
)

type Reconciler struct {
	client.Client
	Reader   client.Reader // uncached: Secrets, ServiceEntries, AuthorizationPolicies
	Recorder recorder.EventRecorder
	Prober   *probe.Prober
	// The profile sync's image (this controller's) and the CA bundle
	// ConfigMap it mounts.
	SyncImage, SyncCAConfigMap string

	mu        sync.Mutex
	brokers   map[types.NamespacedName]*keycloak.Client
	discovery map[string]*probe.Discovery // last good discovery per instance/tier
	profiles  map[types.NamespacedName]profileMark
	signIn    map[types.NamespacedName]signInMark // when each broker's sign-in clients were last read
}

type signInMark struct {
	at         time.Time
	generation int64
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.brokers = map[types.NamespacedName]*keycloak.Client{}
	r.discovery = map[string]*probe.Discovery{}
	r.profiles = map[types.NamespacedName]profileMark{}
	r.signIn = map[types.NamespacedName]signInMark{}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.IdentityContinuity{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Secrets are not watched: listing or watching them would mean reading
		// every Secret in the namespace. Credentials are read by name on each
		// reconcile (every health interval), so a rotation takes effect within
		// one interval, and RBAC grants get on those names only.
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ic v1.IdentityContinuity
	if err := r.Get(ctx, req.NamespacedName, &ic); err != nil {
		if apierrors.IsNotFound(err) {
			r.forget(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	kc, brokerErr := r.broker(ctx, &ic)
	if !ic.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &ic, kc, brokerErr)
	}
	if controllerutil.AddFinalizer(&ic, finalizer) {
		if err := r.Update(ctx, &ic); err != nil {
			return ctrl.Result{}, err
		}
	}
	interval := time.Duration(max(ic.Spec.Health.IntervalSeconds, 1)) * time.Second
	orig := ic.DeepCopy()

	creds, err := r.tierCredentials(ctx, &ic)
	if err != nil {
		return ctrl.Result{}, err
	}
	broker := r.probeAll(ctx, &ic, creds, kc != nil, interval)
	// after the probes, so each tier's ServiceEntry covers the endpoints its
	// discovery names
	egressErr := r.reconcileEgress(ctx, &ic, ic.Spec.Tiers)
	r.markPartitions(ctx, &ic)
	r.checkTrust(ctx, &ic, creds)
	r.refreshSignIn(ctx, &ic, kc)

	byName := map[string]*v1.TierStatus{}
	for i := range ic.Status.Tiers {
		byName[ic.Status.Tiers[i].Name] = &ic.Status.Tiers[i]
	}
	active, why := tiers.Select(ic.Spec, byName, ic.Status.Active)

	// Status says where logins go only once Keycloak does: with the broker
	// unreachable (a restart, say) nothing can change, so hold; a Manual
	// failback isn't undone just because the local tier's health is the
	// broker's.
	kcErr := brokerErr
	profileErr := r.reconcileSync(ctx, &ic)
	switch {
	case kc == nil:
	case broker.Kind != tiers.Healthy:
		kcErr = fmt.Errorf("broker unreachable: %s", broker.Message)
	default:
		effective, applied, _, err := r.reconcileKeycloak(ctx, &ic, kc, creds, byName, active)
		kcErr = err
		// after the failover path, and never holding it up
		profileErr = errors.Join(profileErr, r.reconcileProfile(ctx, &ic, kc))
		if applied {
			if effective != active {
				why = fmt.Sprintf("%s; %q could not be set up in Keycloak", why, active)
			}
			r.recordActive(&ic, effective, why)
		}
	}
	r.setConditions(&ic, byName, active, kcErr, egressErr)
	r.setProfileCondition(&ic, profileErr)
	r.setTrustCondition(&ic)
	ic.Status.ObservedGeneration = ic.Generation
	if err := r.Status().Patch(ctx, &ic, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

// broker returns the Keycloak admin client with current credentials, or nil
// and the reason it can't be used.
func (r *Reconciler) broker(ctx context.Context, ic *v1.IdentityContinuity) (*keycloak.Client, error) {
	b := ic.Spec.Broker.Keycloak
	var s corev1.Secret
	if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: ic.Namespace, Name: b.CredentialsRef.Name}, &s); err != nil {
		return nil, fmt.Errorf("broker credentials: %w", err)
	}
	id, secret := string(s.Data["client-id"]), string(s.Data["client-secret"])
	if id == "" || secret == "" {
		return nil, fmt.Errorf("broker credentials: secret %s needs client-id and client-secret", b.CredentialsRef.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := client.ObjectKeyFromObject(ic)
	kc := r.brokers[key]
	if kc == nil || kc.Base() != strings.TrimSuffix(b.URL, "/") || kc.Realm() != b.Realm {
		kc = keycloak.New(b.URL, b.Realm)
		r.brokers[key] = kc
	}
	kc.SetCredentials(id, secret)
	return kc, nil
}

func (r *Reconciler) forget(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.brokers, key)
	delete(r.profiles, key)
	for k := range r.discovery {
		if strings.HasPrefix(k, key.String()+"/") {
			delete(r.discovery, k)
		}
	}
}

type credential struct{ id, secret, missing string }

// tierCredentials reads each oidc tier's client credentials. A missing Secret
// or key makes the tier NotConfigured; any other read error is retried.
func (r *Reconciler) tierCredentials(ctx context.Context, ic *v1.IdentityContinuity) (map[string]credential, error) {
	out := map[string]credential{}
	for _, t := range ic.Spec.Tiers {
		if t.Type != "oidc" || t.OIDC == nil {
			continue
		}
		if t.OIDC.ClientAuth == "private_key_jwt" { // the realm's key, no secret
			c := credential{id: t.OIDC.ClientID}
			if c.id == "" {
				c.missing = "clientAuth private_key_jwt needs clientID"
			}
			out[t.Name] = c
			continue
		}
		if t.OIDC.ClientSecretRef == nil {
			out[t.Name] = credential{missing: "no clientSecretRef"}
			continue
		}
		ref := *t.OIDC.ClientSecretRef
		key := ref.Key
		if key == "" {
			key = "client-secret"
		}
		var s corev1.Secret
		err := r.Reader.Get(ctx, types.NamespacedName{Namespace: ic.Namespace, Name: ref.Name}, &s)
		switch {
		case apierrors.IsNotFound(err):
			out[t.Name] = credential{missing: fmt.Sprintf("secret %s/%s not found", ic.Namespace, ref.Name)}
			continue
		case err != nil:
			return nil, err
		}
		c := credential{id: t.OIDC.ClientID, secret: string(s.Data[key])}
		if c.id == "" {
			c.id = string(s.Data["client-id"])
		}
		switch {
		case c.secret == "":
			c.missing = fmt.Sprintf("secret %s/%s has no %s", ic.Namespace, ref.Name, key)
		case c.id == "":
			c.missing = fmt.Sprintf("no clientID in the tier or client-id in secret %s", ref.Name)
		}
		out[t.Name] = c
	}
	return out, nil
}

// probeAll probes every tier concurrently and folds the results into status.
// A probe counts toward the thresholds at most once per interval (tiers.Due),
// however often reconciles run. It returns the broker's own probe.
func (r *Reconciler) probeAll(ctx context.Context, ic *v1.IdentityContinuity, creds map[string]credential, brokerCreds bool, interval time.Duration) tiers.Result {
	timeout := time.Duration(max(ic.Spec.Health.TimeoutSeconds, 1)) * time.Second
	prev := map[string]*v1.TierStatus{}
	for i := range ic.Status.Tiers {
		prev[ic.Status.Tiers[i].Name] = &ic.Status.Tiers[i]
	}
	b := ic.Spec.Broker.Keycloak
	broker, issuer := r.Prober.Broker(ctx, strings.TrimSuffix(b.URL, "/")+"/realms/"+url.PathEscape(b.Realm)+"/.well-known/openid-configuration", timeout)
	if broker.Kind == tiers.Healthy && !brokerCreds {
		broker = tiers.Result{Kind: tiers.NotConfigured, Message: "broker credentials missing", Latency: broker.Latency}
	}
	if issuer != "" {
		var signIn []v1.SignInClient
		if ic.Status.Broker != nil {
			signIn = ic.Status.Broker.SignIn // read with the trust checks' cadence (refreshSignIn)
		}
		ic.Status.Broker = &v1.BrokerStatus{Issuer: issuer, SignIn: signIn}
	}
	results := make([]tiers.Result, len(ic.Spec.Tiers))
	var wg sync.WaitGroup
	for i, t := range ic.Spec.Tiers {
		if t.Type == "local" {
			results[i] = broker // a local tier is exactly as healthy as its broker
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, d := r.Prober.OIDC(ctx, t.OIDC.Issuer, timeout)
			results[i] = res
			if d != nil {
				r.mu.Lock()
				r.discovery[discoveryKey(ic, t)] = d
				r.mu.Unlock()
			}
		}()
	}
	wg.Wait()
	r.pruneDiscovery(ic)

	now := metav1.Now()
	out := make([]v1.TierStatus, 0, len(ic.Spec.Tiers))
	for i, t := range ic.Spec.Tiers {
		res := results[i]
		p := prev[t.Name]
		if p != nil && p.Type != t.Type {
			p = nil
		}
		failed := res.Kind == tiers.NotConfigured || tiers.Counts(res, t.FailoverWhen)
		last := &now
		var healthy bool
		var fails, succ int32
		if tiers.Due(p, now.Time, interval) {
			healthy, fails, succ = tiers.Advance(p, failed, ic.Spec.Health)
		} else {
			healthy, fails, succ, last = p.Healthy, p.ConsecutiveFailures, p.ConsecutiveSuccesses, p.LastProbe
		}
		st := v1.TierStatus{
			Name: t.Name, Type: t.Type, Configured: true, Healthy: healthy,
			LatencyMs: res.Latency.Milliseconds(), LastProbe: last,
			Reason: res.Kind, Message: res.Message, ConsecutiveFailures: fails, ConsecutiveSuccesses: succ,
		}
		if res.Kind == tiers.NotConfigured {
			st.Configured = false
		}
		if t.Type == "oidc" && ic.Status.Broker != nil {
			st.RedirectURI = ic.Status.Broker.Issuer + "/broker/" + t.Name + "/endpoint"
		}
		if p != nil && t.Type == "oidc" {
			st.Trust = p.Trust // kept until checkTrust runs again
		}
		switch {
		case res.Kind == tiers.Healthy && failed:
			st.Reason, st.Message = tiers.SlowResponse, fmt.Sprintf("%dms is above latencyAboveMs %d", st.LatencyMs, *t.FailoverWhen.LatencyAboveMs)
		case res.Kind != tiers.Healthy && !failed:
			st.Message += " (ignored by failoverWhen)"
		}
		if c, ok := creds[t.Name]; ok && c.missing != "" {
			// Still probed, so the rule builder can show the upstream's health.
			st.Configured, st.Reason, st.Message = false, tiers.NotConfigured, c.missing+"; probe: "+res.Kind
		}
		if p != nil && p.Healthy != st.Healthy {
			if st.Healthy {
				r.Recorder.Eventf(ic, nil, corev1.EventTypeNormal, "TierHealthy", "Probe", "tier %s is healthy (%s)", t.Name, st.Message)
			} else {
				r.Recorder.Eventf(ic, nil, corev1.EventTypeWarning, "TierUnhealthy", "Probe", "tier %s is unhealthy: %s: %s", t.Name, st.Reason, st.Message)
			}
		}
		out = append(out, st)
	}
	ic.Status.Tiers = out
	return broker
}

// refreshSignIn reads the broker's sign-in clients when the spec changed or
// trustEvery passed: the apps that sign people in through it, for whoever
// draws the sign-in paths (status.broker.signIn). A failed read keeps the last.
func (r *Reconciler) refreshSignIn(ctx context.Context, ic *v1.IdentityContinuity, kc *keycloak.Client) {
	if kc == nil || ic.Status.Broker == nil {
		return
	}
	key := client.ObjectKeyFromObject(ic)
	r.mu.Lock()
	m := r.signIn[key]
	r.mu.Unlock()
	if m.generation == ic.Generation && time.Since(m.at) < trustEvery && ic.Status.Broker.SignIn != nil {
		return
	}
	cl, err := kc.SignInClients(ctx)
	if err != nil {
		log.FromContext(ctx).Info("broker sign-in clients unread", "err", err.Error())
		return
	}
	out := make([]v1.SignInClient, 0, len(cl))
	for _, c := range cl {
		out = append(out, v1.SignInClient{ClientID: c.ClientID, RedirectURIs: c.RedirectURIs})
	}
	ic.Status.Broker.SignIn = out
	r.mu.Lock()
	r.signIn[key] = signInMark{at: time.Now(), generation: ic.Generation}
	r.mu.Unlock()
}

// checkTrust runs each oidc tier's trust checks when due: never checked, the
// spec changed, the check-trust annotation changed, or trustEvery passed.
// Only with the IdP's discovery in hand and its client known; otherwise the
// last result stays.
func (r *Reconciler) checkTrust(ctx context.Context, ic *v1.IdentityContinuity, creds map[string]credential) {
	timeout := time.Duration(max(ic.Spec.Health.TimeoutSeconds, 1)) * time.Second
	req := ic.Annotations[AnnotationCheckTrust]
	now := metav1.Now()
	spec := map[string]v1.Tier{}
	for _, t := range ic.Spec.Tiers {
		spec[t.Name] = t
	}
	var wg sync.WaitGroup
	for i := range ic.Status.Tiers {
		st := &ic.Status.Tiers[i]
		t, ok := spec[st.Name]
		if !ok || t.Type != "oidc" || t.OIDC == nil {
			continue
		}
		d, id := r.cachedDiscovery(ic, t), creds[t.Name].id
		if d == nil || id == "" || st.RedirectURI == "" {
			continue
		}
		if tr := st.Trust; tr != nil && tr.Generation == ic.Generation && tr.Requested == req && now.Sub(tr.CheckedAt.Time) < trustEvery {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			was := map[string]bool{}
			if st.Trust != nil {
				for _, f := range trust.Failed(st.Trust.Checks) {
					was[f] = true
				}
			}
			cb, msg := r.Prober.Callback(ctx, d.AuthorizationEndpoint, id, st.RedirectURI, timeout)
			checks := trust.Checks(t, d, cb, msg)
			st.Trust = &v1.TrustStatus{CheckedAt: now, Generation: ic.Generation, Requested: req, Checks: checks}
			for _, c := range checks {
				if c.Result == trust.Fail && !was[c.Name] {
					r.Recorder.Eventf(ic, nil, corev1.EventTypeWarning, "TrustMismatch", "CheckTrust", "IdP %s: %s: %s", t.Name, c.Name, truncate(c.Message, 512))
				}
			}
		}()
	}
	wg.Wait()
}

// setTrustCondition: TrustConsistent is False while any IdP fails a trust
// check; Unknown results (not published) don't count against it.
func (r *Reconciler) setTrustCondition(ic *v1.IdentityContinuity) {
	c := metav1.Condition{Type: "TrustConsistent", Status: metav1.ConditionTrue, Reason: "Consistent", ObservedGeneration: ic.Generation,
		Message: "every IdP checked accepts the broker's registration"}
	var bad []string
	checked := 0
	for _, st := range ic.Status.Tiers {
		if st.Trust == nil {
			continue
		}
		checked++
		for _, ch := range st.Trust.Checks {
			if ch.Result == trust.Fail {
				bad = append(bad, fmt.Sprintf("%s %s: %s", st.Name, ch.Name, ch.Message))
			}
		}
	}
	switch {
	case len(bad) > 0:
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "RegistrationMismatch", strings.Join(bad, "; ")
	case checked == 0:
		c.Status, c.Reason, c.Message = metav1.ConditionUnknown, "NotChecked", "no IdP checked yet"
	}
	c.Message = truncate(c.Message, maxMessage)
	meta.SetStatusCondition(&ic.Status.Conditions, c)
}

func discoveryKey(ic *v1.IdentityContinuity, t v1.Tier) string {
	return client.ObjectKeyFromObject(ic).String() + "/" + t.Name + "|" + t.OIDC.Issuer
}

func (r *Reconciler) cachedDiscovery(ic *v1.IdentityContinuity, t v1.Tier) *probe.Discovery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.discovery[discoveryKey(ic, t)]
}

// pruneDiscovery drops the instance's cached discovery for tiers (or
// issuers) no longer in the spec.
func (r *Reconciler) pruneDiscovery(ic *v1.IdentityContinuity) {
	keep := map[string]bool{}
	for _, t := range ic.Spec.Tiers {
		if t.OIDC != nil {
			keep[discoveryKey(ic, t)] = true
		}
	}
	prefix := client.ObjectKeyFromObject(ic).String() + "/"
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.discovery {
		if strings.HasPrefix(k, prefix) && !keep[k] {
			delete(r.discovery, k)
		}
	}
}

// markPartitions flags tiers that have a partition policy. Informational only.
// A partition counts only where it can cut this instance's path: a DENY in its
// egress namespace that targets the tier's own ServiceEntry. A policy elsewhere
// carrying the tier label says nothing about S&V's path.
func (r *Reconciler) markPartitions(ctx context.Context, ic *v1.IdentityContinuity) {
	for i := range ic.Status.Tiers {
		ic.Status.Tiers[i].Partitioned = false
	}
	if ic.Spec.Egress == nil {
		return
	}
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(authzPolicyGVK.GroupVersion().WithKind("AuthorizationPolicyList"))
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: LabelTier, Operator: metav1.LabelSelectorOpExists}}})
	if err := r.Reader.List(ctx, &list, client.InNamespace(ic.Spec.Egress.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		log.FromContext(ctx).V(1).Info("listing partition policies", "err", err.Error())
		return
	}
	cut := map[string]bool{}
	for _, p := range list.Items {
		tier := p.GetLabels()[LabelTier]
		if a, _, _ := unstructured.NestedString(p.Object, "spec", "action"); a == "DENY" && targetsServiceEntry(p, serviceEntryName(tier)) {
			cut[tier] = true
		}
	}
	for i := range ic.Status.Tiers {
		ic.Status.Tiers[i].Partitioned = cut[ic.Status.Tiers[i].Name]
	}
}

// serviceEntryName is the ServiceEntry reconcileEgress keeps for a tier.
func serviceEntryName(tier string) string { return "continuity-" + tier }

func targetsServiceEntry(p unstructured.Unstructured, name string) bool {
	refs, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs")
	for _, r := range refs {
		m, _ := r.(map[string]any)
		if m["kind"] == "ServiceEntry" && m["name"] == name {
			return true
		}
	}
	return false
}

func (r *Reconciler) recordActive(ic *v1.IdentityContinuity, active, why string) {
	from := ic.Status.Active
	if from == active && ic.Status.ActiveSince != nil {
		return
	}
	now := metav1.Now()
	reason := tiers.Direction(ic.Spec, from, active)
	msg := fmt.Sprintf("logins now go to %q (was %q): %s", active, from, why)
	if active == "" {
		msg = "no tier can take logins; the realm's own login form is shown"
	}
	ic.Status.Active, ic.Status.ActiveSince = active, &now
	ic.Status.Transitions = append(ic.Status.Transitions, v1.Transition{Time: now, From: from, To: active, Reason: reason})
	if n := len(ic.Status.Transitions); n > maxHistory {
		ic.Status.Transitions = ic.Status.Transitions[n-maxHistory:]
	}
	typ := corev1.EventTypeNormal
	if reason == "FailoverActivated" {
		typ = corev1.EventTypeWarning
	}
	r.Recorder.Eventf(ic, nil, typ, reason, "SelectTier", "%s", msg)
}

func (r *Reconciler) setConditions(ic *v1.IdentityContinuity, st map[string]*v1.TierStatus, active string, kcErr, egressErr error) {
	ready := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled", Message: "Keycloak matches the active tier"}
	if err := errors.Join(kcErr, egressErr); err != nil {
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, "ReconcileFailed", err.Error()
	}
	degraded := metav1.Condition{Type: "Degraded", Status: metav1.ConditionFalse, Reason: "PreferredTier", Message: "the first tier in the chain is active"}
	for _, t := range ic.Spec.Tiers {
		if (t.Enabled != nil && !*t.Enabled) || st[t.Name] == nil || !st[t.Name].Configured {
			continue // not expected to carry logins
		}
		if t.Name != active {
			degraded.Status, degraded.Reason = metav1.ConditionTrue, "FailedOver"
			degraded.Message = fmt.Sprintf("preferred tier %q is not active; %q is", t.Name, active)
		}
		break
	}
	rules := metav1.Condition{Type: "RulesEffective", Status: metav1.ConditionTrue, Reason: "Effective", Message: "every failover rule can fire"}
	timeoutMs := max(ic.Spec.Health.TimeoutSeconds, 1) * 1000
	for _, t := range ic.Spec.Tiers {
		if l := t.FailoverWhen.LatencyAboveMs; l != nil && *l >= timeoutMs {
			rules.Status, rules.Reason = metav1.ConditionFalse, "LatencyAboveTimeout"
			rules.Message = fmt.Sprintf("tier %s: latencyAboveMs %d is not below the %dms probe timeout, so it never fires (a slower answer is Unreachable)", t.Name, *l, timeoutMs)
			break
		}
	}
	for _, c := range []metav1.Condition{ready, degraded, rules} {
		c.ObservedGeneration = ic.Generation
		c.Message = truncate(c.Message, maxMessage)
		meta.SetStatusCondition(&ic.Status.Conditions, c)
	}
}

// truncate cuts s to at most n bytes, on a character boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n-3], "") + "..."
}

// setProfileCondition reports the profile, the attribute mappings and the
// sync's CronJob: separate from Ready, which is about where sign-ins go.
func (r *Reconciler) setProfileCondition(ic *v1.IdentityContinuity, err error) {
	c := metav1.Condition{Type: "ProfileApplied", Status: metav1.ConditionTrue, Reason: "Applied", ObservedGeneration: ic.Generation,
		Message: "the profile, attribute mappings and sync schedule are in place"}
	switch {
	case err != nil:
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "Failed", err.Error()
	case ic.Spec.Profile == nil && ic.Spec.Sync == nil && !hasMappings(ic):
		c.Reason, c.Message = "NotConfigured", "no profile, attribute mappings or sync"
	}
	c.Message = truncate(c.Message, maxMessage)
	meta.SetStatusCondition(&ic.Status.Conditions, c)
}

func hasMappings(ic *v1.IdentityContinuity) bool {
	for _, t := range ic.Spec.Tiers {
		if len(t.Attributes) > 0 {
			return true
		}
	}
	return false
}

// reconcileKeycloak: one OIDC IdP per configured oidc tier (hidden on the
// login page unless eligible), the redirector on the active tier, and no
// IdPs this instance owns beyond those. It returns the tier logins actually
// go to now, whether the redirector was set (so status can follow it), and
// the tiers whose IdP exists.
func (r *Reconciler) reconcileKeycloak(ctx context.Context, ic *v1.IdentityContinuity, kc *keycloak.Client, creds map[string]credential, st map[string]*v1.TierStatus, active string) (string, bool, map[string]bool, error) {
	existing, err := kc.IdPs(ctx)
	if err != nil {
		return "", false, nil, err
	}
	have := map[string]keycloak.IdP{}
	for _, p := range existing {
		have[fmt.Sprint(p["alias"])] = p
	}
	owner := client.ObjectKeyFromObject(ic).String()
	owned := func(p keycloak.IdP) bool { return p.Config()[cfgInstance] == owner }

	var errs []error
	keep := map[string]bool{}
	redirect := ""
	for _, t := range ic.Spec.Tiers {
		c, ok := creds[t.Name]
		if t.Type != "oidc" || !ok {
			continue
		}
		cur, exists := have[t.Name]
		if exists && !owned(cur) {
			errs = append(errs, fmt.Errorf("identity provider %q exists and is not managed by %s", t.Name, owner))
			continue
		}
		if c.missing != "" {
			// Credentials gone, perhaps only for a moment: keep the IdP (and
			// every user's link to it), just stop offering it. Only removing
			// the tier from the spec deletes it.
			if exists {
				keep[t.Name] = true
				if cur["hideOnLogin"] != true {
					cur["hideOnLogin"] = true
					errs = append(errs, kc.UpdateIdP(ctx, t.Name, cur))
				}
			}
			continue
		}
		hide := !tiers.Eligible(t, st[t.Name])
		// Only the active tier signs anyone in: a hint or a direct broker URL
		// can't reach an upstream that failed over, drained or is waiting
		// its turn. Users' links to it stay.
		enabled := (t.Enabled == nil || *t.Enabled) && t.Name == active
		var err error
		d := r.cachedDiscovery(ic, t)
		switch {
		case d != nil:
			want := desiredIdP(ic, t, c, d, owner, hide, enabled)
			if !exists {
				err = kc.CreateIdP(ctx, want)
			} else if !sameIdP(cur, want) {
				// only what the controller manages: other fields and config
				// keys set in Keycloak stay
				for _, k := range idpManaged {
					cur[k] = want[k]
				}
				cc := cur.Config()
				for k, v := range want.Config() {
					cc[k] = v
				}
				err = kc.UpdateIdP(ctx, t.Name, cur)
			}
		case exists:
			// Upstream not seen since this controller started: keep the IdP as
			// it is (Keycloak keeps the masked secret) and just fix visibility.
			if cur["hideOnLogin"] != hide || cur["enabled"] != enabled {
				cur["hideOnLogin"], cur["enabled"] = hide, enabled
				err = kc.UpdateIdP(ctx, t.Name, cur)
			}
		default:
			continue
		}
		keep[t.Name] = true
		if err == nil {
			err = kc.EnsureMapper(ctx, t.Name, keycloak.UsernameMapper)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if t.Name == active {
			redirect = t.Name // only ever point logins at an IdP that exists
		}
		// Never holds up sign-ins: without it a session carries no upstream
		// acr or amr, so workloads that require more than AAL1 refuse it
		// (fail closed) while Ready says why.
		if err := kc.EnsureMapper(ctx, t.Name, keycloak.AssuranceMapper); err != nil {
			errs = append(errs, fmt.Errorf("identity provider %s: assurance mapper: %w", t.Name, err))
		}
		if err := ensureGroupMappers(ctx, kc, ic, t); err != nil {
			errs = append(errs, fmt.Errorf("identity provider %s: group mappers: %w", t.Name, err))
		}
	}
	flow := ic.Spec.Broker.Keycloak.BrowserFlow
	changed, rerr := kc.SetRedirector(ctx, flow, redirect)
	if rerr != nil {
		errs = append(errs, rerr)
	} else if changed {
		log.FromContext(ctx).Info("redirector updated", "flow", flow, "defaultProvider", redirect)
	}
	for alias, p := range have {
		if owned(p) && !keep[alias] {
			errs = append(errs, kc.DeleteIdP(ctx, alias))
		}
	}
	effective := redirect
	if effective == "" {
		// no upstream redirect: the realm's own form, i.e. the local tier
		effective, _ = tiers.Select(v1.IdentityContinuitySpec{Tiers: localTiers(ic.Spec.Tiers)}, nil, "")
	}
	return effective, rerr == nil, keep, errors.Join(errs...)
}

func localTiers(ts []v1.Tier) []v1.Tier {
	var out []v1.Tier
	for _, t := range ts {
		if t.Type == "local" {
			out = append(out, t)
		}
	}
	return out
}

func desiredIdP(ic *v1.IdentityContinuity, t v1.Tier, c credential, d *probe.Discovery, owner string, hide, enabled bool) keycloak.IdP {
	scopes := t.OIDC.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}
	name := t.DisplayName
	if name == "" {
		name = t.Name
	}
	cfg := map[string]any{
		"issuer":            t.OIDC.Issuer,
		"authorizationUrl":  d.AuthorizationEndpoint,
		"tokenUrl":          d.TokenEndpoint,
		"jwksUrl":           d.JWKSURI,
		"useJwksUrl":        "true",
		"validateSignature": "true",
		"clientId":          c.id,
		"defaultScope":      strings.Join(scopes, " "),
		"pkceEnabled":       "true",
		"pkceMethod":        "S256",
		"syncMode":          "IMPORT",
		// Auto-linking by email is only safe for addresses the upstream verified.
		"filteredByClaim":  "true",
		"claimFilterName":  "email_verified",
		"claimFilterValue": "true",
		cfgInstance:        owner,
	}
	switch auth := t.OIDC.ClientAuth; auth {
	case "private_key_jwt":
		alg := t.OIDC.ClientAssertionSigningAlg
		if alg == "" {
			alg = "PS256"
		}
		cfg["clientAuthMethod"] = auth
		cfg["clientAssertionSigningAlg"] = alg
	case "client_secret_basic":
		cfg["clientAuthMethod"] = auth
		cfg["clientSecret"] = c.secret
	default:
		cfg["clientAuthMethod"] = "client_secret_post"
		cfg["clientSecret"] = c.secret
	}
	if d.UserinfoEndpoint != "" {
		cfg["userInfoUrl"] = d.UserinfoEndpoint
	}
	if d.EndSessionEndpoint != "" {
		cfg["logoutUrl"] = d.EndSessionEndpoint
	}
	p := keycloak.IdP{
		"alias": t.Name, "displayName": name, "providerId": "oidc",
		"enabled": enabled, "hideOnLogin": hide, "trustEmail": true, "storeToken": t.OIDC.StoreTokens,
		"firstBrokerLoginFlowAlias": ic.Spec.Broker.Keycloak.FirstBrokerLoginFlow,
		"config":                    cfg,
	}
	b, _ := json.Marshal(p) // map keys marshal sorted: a stable hash
	sum := sha256.Sum256(b)
	cfg[cfgHash] = hex.EncodeToString(sum[:8])
	return p
}

// idpManaged are the top-level IdP fields the controller sets (besides its
// config keys).
var idpManaged = []string{"displayName", "providerId", "enabled", "hideOnLogin", "trustEmail", "storeToken", "firstBrokerLoginFlowAlias"}

// sameIdP compares what the controller manages on an IdP with what Keycloak
// has now, so a change made in Keycloak by hand is put back. The client
// secret comes back masked; the hash (which covers it) stands in for it.
func sameIdP(cur, want keycloak.IdP) bool {
	for _, k := range idpManaged {
		if fmt.Sprint(cur[k]) != fmt.Sprint(want[k]) {
			return false
		}
	}
	cc := cur.Config()
	for k, v := range want.Config() {
		if k != "clientSecret" && fmt.Sprint(cc[k]) != fmt.Sprint(v) {
			return false
		}
	}
	return true
}

// reconcileEgress keeps one ServiceEntry per external oidc tier, bound to the
// egress waypoint: that is where S&V's back-channel to the upstream leaves the
// mesh, so it is where policy (and a partition) applies. It never takes over
// another instance's ServiceEntry, and when spec.egress goes away or moves it
// removes what it left in the old namespace (status.egressNamespace).
func (r *Reconciler) reconcileEgress(ctx context.Context, ic *v1.IdentityContinuity, ts []v1.Tier) error {
	e := ic.Spec.Egress
	var errs []error
	if old := ic.Status.EgressNamespace; old != "" && (e == nil || e.Namespace != old) {
		if err := r.pruneServiceEntries(ctx, ic, old, nil); err != nil {
			errs = append(errs, err)
		} else {
			ic.Status.EgressNamespace = ""
		}
	}
	if e == nil {
		return errors.Join(errs...)
	}
	instance := instanceOf(ic)
	want := map[string]*unstructured.Unstructured{}
	for _, t := range ts {
		if t.Type != "oidc" || t.OIDC == nil {
			continue
		}
		hosts := externalHosts(e.InternalDomains, t.OIDC.Issuer, r.cachedDiscovery(ic, t))
		if t.Directory != nil {
			hosts = mergeHosts(hosts, externalHosts(e.InternalDomains, t.Directory.URL, nil))
		}
		if len(hosts) > 0 {
			want[serviceEntryName(t.Name)] = serviceEntry(e, ic.Namespace, instance, t.Name, hosts)
		}
	}
	for name, se := range want {
		var cur unstructured.Unstructured
		cur.SetGroupVersionKind(serviceEntryGVK)
		err := r.Reader.Get(ctx, types.NamespacedName{Namespace: e.Namespace, Name: name}, &cur)
		if err == nil && cur.GetLabels()[LabelInstance] != instance {
			errs = append(errs, fmt.Errorf("ServiceEntry %s/%s belongs to %q, not this instance", e.Namespace, name, cur.GetLabels()[LabelInstance]))
			continue
		}
		errs = append(errs, r.Apply(ctx, client.ApplyConfigurationFromUnstructured(se), client.FieldOwner("continuity-controller"), client.ForceOwnership))
	}
	errs = append(errs, r.pruneServiceEntries(ctx, ic, e.Namespace, want))
	ic.Status.EgressNamespace = e.Namespace
	return errors.Join(errs...)
}

func instanceOf(ic *v1.IdentityContinuity) string { return ic.Namespace + "." + ic.Name }

// pruneServiceEntries deletes this instance's ServiceEntries in ns that aren't in keep.
func (r *Reconciler) pruneServiceEntries(ctx context.Context, ic *v1.IdentityContinuity, ns string, keep map[string]*unstructured.Unstructured) error {
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(serviceEntryGVK.GroupVersion().WithKind("ServiceEntryList"))
	if err := r.Reader.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{LabelInstance: instanceOf(ic)}); err != nil {
		return err
	}
	var errs []error
	for i := range list.Items {
		if keep[list.Items[i].GetName()] == nil {
			errs = append(errs, client.IgnoreNotFound(r.Delete(ctx, &list.Items[i])))
		}
	}
	return errors.Join(errs...)
}

func externalHosts(internal []string, issuer string, d *probe.Discovery) []string {
	urls := []string{issuer}
	if d != nil {
		urls = append(urls, d.TokenEndpoint, d.JWKSURI, d.UserinfoEndpoint)
	}
	set := map[string]bool{}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || isInternal(internal, u.Hostname()) || isClusterService(u.Hostname()) {
			continue
		}
		set[u.Hostname()] = true
	}
	hosts := make([]string, 0, len(set))
	for h := range set {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts
}

func mergeHosts(a, b []string) []string {
	for _, h := range b {
		if !slices.Contains(a, h) {
			a = append(a, h)
		}
	}
	sort.Strings(a)
	return a
}

// isClusterService: a Service's in-cluster name, never routed by the egress.
func isClusterService(host string) bool {
	return strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local")
}

func isInternal(domains []string, host string) bool {
	for _, d := range domains {
		d = strings.Trim(d, ".")
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

func serviceEntry(e *v1.Egress, callerNS, instance, tier string, hosts []string) *unstructured.Unstructured {
	// "." is the egress namespace itself: named once, every other namespace once
	exportTo := []string{"."}
	for _, ns := range append([]string{callerNS}, e.ExportTo...) {
		if ns != "" && ns != e.Namespace && !slices.Contains(exportTo, ns) {
			exportTo = append(exportTo, ns)
		}
	}
	se := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"hosts":      toAny(hosts),
			"exportTo":   toAny(exportTo),
			"location":   "MESH_EXTERNAL",
			"resolution": "DNS",
			"ports":      []any{map[string]any{"number": int64(443), "name": "tls", "protocol": "TLS"}},
		},
	}}
	se.SetGroupVersionKind(serviceEntryGVK)
	se.SetNamespace(e.Namespace)
	se.SetName(serviceEntryName(tier))
	se.SetLabels(map[string]string{"istio.io/use-waypoint": e.Waypoint, LabelTier: tier, LabelInstance: instance})
	return se
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// finalize cleans up on deletion. If Keycloak can't be reached (or its
// credentials are gone) for longer than cleanupGrace, it gives up on
// Keycloak, says so in an event, and lets the object go.
func (r *Reconciler) finalize(ctx context.Context, ic *v1.IdentityContinuity, kc *keycloak.Client, brokerErr error) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ic, finalizer) {
		return ctrl.Result{}, nil
	}
	err := brokerErr
	if err == nil {
		err = r.cleanup(ctx, ic, kc)
	}
	// the egress is the cluster's own: cleaned whatever Keycloak's state
	err = errors.Join(err, r.cleanupEgress(ctx, ic))
	if err != nil {
		if time.Since(ic.DeletionTimestamp.Time) < cleanupGrace {
			log.FromContext(ctx).Info("cleanup failed, retrying", "err", err.Error())
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		r.Recorder.Eventf(ic, nil, corev1.EventTypeWarning, "CleanupAbandoned", "Delete",
			"gave up cleaning Keycloak after %s; its identity providers and redirector may remain: %v", cleanupGrace, err)
	}
	controllerutil.RemoveFinalizer(ic, finalizer)
	r.forget(client.ObjectKeyFromObject(ic))
	return ctrl.Result{}, r.Update(ctx, ic)
}

// cleanup puts the realm back to plain local login and removes the
// identity providers and profile attributes this instance created.
func (r *Reconciler) cleanup(ctx context.Context, ic *v1.IdentityContinuity, kc *keycloak.Client) error {
	// a flow already gone has no redirector to clear
	if _, err := kc.SetRedirector(ctx, ic.Spec.Broker.Keycloak.BrowserFlow, ""); err != nil && !errors.Is(err, keycloak.ErrNotFound) {
		return err
	}
	if err := cleanupProfile(ctx, ic, kc); err != nil {
		return err
	}
	idps, err := kc.IdPs(ctx)
	if err != nil {
		return err
	}
	owner := client.ObjectKeyFromObject(ic).String()
	for _, p := range idps {
		if p.Config()[cfgInstance] == owner {
			if err := kc.DeleteIdP(ctx, fmt.Sprint(p["alias"])); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Reconciler) cleanupEgress(ctx context.Context, ic *v1.IdentityContinuity) error {
	var errs []error
	seen := map[string]bool{}
	for _, ns := range []string{ic.Status.EgressNamespace, egressNamespace(ic)} {
		if ns != "" && !seen[ns] {
			seen[ns] = true
			errs = append(errs, r.pruneServiceEntries(ctx, ic, ns, nil))
		}
	}
	return errors.Join(errs...)
}

func egressNamespace(ic *v1.IdentityContinuity) string {
	if ic.Spec.Egress == nil {
		return ""
	}
	return ic.Spec.Egress.Namespace
}

// ensureGroupMappers keeps one mapper per shape group on the tier's IdP, from
// its groups claim (tiers[].groups), and removes the rest: membership of the
// shape's groups always comes from the IdP a user signs in through. The
// groups themselves are the directory sync's to create (it holds
// manage-users); a mapper whose group isn't there yet maps nothing.
func ensureGroupMappers(ctx context.Context, kc *keycloak.Client, ic *v1.IdentityContinuity, t v1.Tier) error {
	keep := map[string]bool{}
	var errs []error
	if t.Groups != nil && ic.Spec.Profile != nil {
		for _, g := range ic.Spec.Profile.Groups {
			m := keycloak.GroupMapper(t.Groups.Claim, g)
			keep[m.Name] = true
			errs = append(errs, kc.EnsureMapper(ctx, t.Name, m))
		}
	}
	errs = append(errs, kc.PruneMappers(ctx, t.Name, keycloak.GroupMapperPrefix, keep))
	return errors.Join(errs...)
}
