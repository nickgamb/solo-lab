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
)

const (
	finalizer     = "continuity.lab.solo.io/cleanup"
	LabelTier     = "continuity.lab.solo.io/tier"
	LabelInstance = "continuity.lab.solo.io/instance"
	cfgInstance   = "continuity.lab.solo.io/instance" // IdP config key: who owns it
	cfgHash       = "continuity.lab.solo.io/hash"
	maxHistory    = 20
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

	mu        sync.Mutex
	brokers   map[types.NamespacedName]*keycloak.Client
	discovery map[string]*probe.Discovery // last good discovery per instance/tier
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.brokers = map[types.NamespacedName]*keycloak.Client{}
	r.discovery = map[string]*probe.Discovery{}
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
		if brokerErr != nil {
			return ctrl.Result{}, brokerErr
		}
		if err := r.cleanup(ctx, &ic, kc); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(&ic, finalizer)
		r.forget(req.NamespacedName)
		return ctrl.Result{}, r.Update(ctx, &ic)
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
	egressErr := r.reconcileEgress(ctx, &ic, ic.Spec.Egress, ic.Spec.Tiers)
	r.probeAll(ctx, &ic, creds, kc != nil)
	r.markPartitions(ctx, &ic)

	byName := map[string]*v1.TierStatus{}
	for i := range ic.Status.Tiers {
		byName[ic.Status.Tiers[i].Name] = &ic.Status.Tiers[i]
	}
	active, why := tiers.Select(ic.Spec, byName, ic.Status.Active)

	kcErr := brokerErr
	if kc != nil {
		kcErr = r.reconcileKeycloak(ctx, &ic, kc, creds, byName, active)
	}
	r.recordActive(&ic, active, why)
	r.setConditions(&ic, byName, active, kcErr, egressErr)
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
		ref := t.OIDC.ClientSecretRef
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
func (r *Reconciler) probeAll(ctx context.Context, ic *v1.IdentityContinuity, creds map[string]credential, brokerCreds bool) {
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
		ic.Status.Broker = &v1.BrokerStatus{Issuer: issuer}
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

	now := metav1.Now()
	out := make([]v1.TierStatus, 0, len(ic.Spec.Tiers))
	for i, t := range ic.Spec.Tiers {
		res := results[i]
		p := prev[t.Name]
		if p != nil && p.Type != t.Type {
			p = nil
		}
		failed := res.Kind == tiers.NotConfigured || tiers.Counts(res, t.FailoverWhen)
		healthy, fails, succ := tiers.Advance(p, failed, ic.Spec.Health)
		st := v1.TierStatus{
			Name: t.Name, Type: t.Type, Configured: true, Healthy: healthy,
			LatencyMs: res.Latency.Milliseconds(), LastProbe: &now,
			Reason: res.Kind, Message: res.Message, ConsecutiveFailures: fails, ConsecutiveSuccesses: succ,
		}
		if res.Kind == tiers.NotConfigured {
			st.Configured = false
		}
		if t.Type == "oidc" && ic.Status.Broker != nil {
			st.RedirectURI = ic.Status.Broker.Issuer + "/broker/" + t.Name + "/endpoint"
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
}

func discoveryKey(ic *v1.IdentityContinuity, t v1.Tier) string {
	return client.ObjectKeyFromObject(ic).String() + "/" + t.Name + "|" + t.OIDC.Issuer
}

func (r *Reconciler) cachedDiscovery(ic *v1.IdentityContinuity, t v1.Tier) *probe.Discovery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.discovery[discoveryKey(ic, t)]
}

// markPartitions flags tiers that have a partition policy. Informational only.
func (r *Reconciler) markPartitions(ctx context.Context, ic *v1.IdentityContinuity) {
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(authzPolicyGVK.GroupVersion().WithKind("AuthorizationPolicyList"))
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: LabelTier, Operator: metav1.LabelSelectorOpExists}}})
	if err := r.Reader.List(ctx, &list, client.MatchingLabelsSelector{Selector: sel}); err != nil {
		log.FromContext(ctx).V(1).Info("listing partition policies", "err", err.Error())
		return
	}
	cut := map[string]bool{}
	for _, p := range list.Items {
		if a, _, _ := unstructured.NestedString(p.Object, "spec", "action"); a == "DENY" {
			cut[p.GetLabels()[LabelTier]] = true
		}
	}
	for i := range ic.Status.Tiers {
		ic.Status.Tiers[i].Partitioned = cut[ic.Status.Tiers[i].Name]
	}
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
		msg = "no tier is eligible and there is no local tier"
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
	for _, c := range []metav1.Condition{ready, degraded} {
		c.ObservedGeneration = ic.Generation
		meta.SetStatusCondition(&ic.Status.Conditions, c)
	}
}

// reconcileKeycloak: one OIDC IdP per configured oidc tier (hidden on the
// login page unless eligible), the redirector on the active tier, and no
// IdPs this instance owns beyond those.
func (r *Reconciler) reconcileKeycloak(ctx context.Context, ic *v1.IdentityContinuity, kc *keycloak.Client, creds map[string]credential, st map[string]*v1.TierStatus, active string) error {
	existing, err := kc.IdPs(ctx)
	if err != nil {
		return err
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
		if t.Type != "oidc" || !ok || c.missing != "" {
			continue
		}
		cur, exists := have[t.Name]
		if exists && !owned(cur) {
			errs = append(errs, fmt.Errorf("identity provider %q exists and is not managed by %s", t.Name, owner))
			continue
		}
		hide := !tiers.Eligible(t, st[t.Name])
		enabled := t.Enabled == nil || *t.Enabled
		var err error
		d := r.cachedDiscovery(ic, t)
		switch {
		case d != nil:
			want := desiredIdP(ic, t, c, d, owner, hide, enabled)
			if !exists {
				err = kc.CreateIdP(ctx, want)
			} else if !sameIdP(cur, want) {
				for k, v := range want {
					cur[k] = v
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
			err = kc.EnsureUsernameFromEmail(ctx, t.Name)
		}
		if err != nil {
			errs = append(errs, err)
		} else if t.Name == active {
			redirect = t.Name // only ever point logins at an IdP that exists
		}
	}
	flow := ic.Spec.Broker.Keycloak.BrowserFlow
	if changed, err := kc.SetRedirector(ctx, flow, redirect); err != nil {
		errs = append(errs, err)
	} else if changed {
		log.FromContext(ctx).Info("redirector updated", "flow", flow, "defaultProvider", redirect)
	}
	for alias, p := range have {
		if owned(p) && !keep[alias] {
			errs = append(errs, kc.DeleteIdP(ctx, alias))
		}
	}
	return errors.Join(errs...)
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
		"clientSecret":      c.secret,
		"clientAuthMethod":  "client_secret_post",
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
	if d.UserinfoEndpoint != "" {
		cfg["userInfoUrl"] = d.UserinfoEndpoint
	}
	if d.EndSessionEndpoint != "" {
		cfg["logoutUrl"] = d.EndSessionEndpoint
	}
	p := keycloak.IdP{
		"alias": t.Name, "displayName": name, "providerId": "oidc",
		"enabled": enabled, "hideOnLogin": hide, "trustEmail": true, "storeToken": false,
		"firstBrokerLoginFlowAlias": ic.Spec.Broker.Keycloak.FirstBrokerLoginFlow,
		"config":                    cfg,
	}
	b, _ := json.Marshal(p) // map keys marshal sorted: a stable hash
	sum := sha256.Sum256(b)
	cfg[cfgHash] = hex.EncodeToString(sum[:8])
	return p
}

func sameIdP(cur, want keycloak.IdP) bool {
	for _, k := range []string{"displayName", "enabled", "hideOnLogin", "firstBrokerLoginFlowAlias"} {
		if cur[k] != want[k] {
			return false
		}
	}
	return cur.Config()[cfgHash] == want.Config()[cfgHash]
}

// reconcileEgress keeps one ServiceEntry per external oidc tier, bound to the
// egress waypoint: that is where S&V's back-channel to the upstream leaves the
// mesh, so it is where policy (and a partition) applies.
func (r *Reconciler) reconcileEgress(ctx context.Context, ic *v1.IdentityContinuity, e *v1.Egress, ts []v1.Tier) error {
	if e == nil {
		return nil
	}
	instance := ic.Namespace + "." + ic.Name
	want := map[string]*unstructured.Unstructured{}
	for _, t := range ts {
		if t.Type != "oidc" || t.OIDC == nil {
			continue
		}
		hosts := externalHosts(e.InternalDomains, t.OIDC.Issuer, r.cachedDiscovery(ic, t))
		if len(hosts) > 0 {
			want["continuity-"+t.Name] = serviceEntry(e, ic.Namespace, instance, t.Name, hosts)
		}
	}
	var errs []error
	for _, se := range want {
		errs = append(errs, r.Apply(ctx, client.ApplyConfigurationFromUnstructured(se), client.FieldOwner("continuity-controller"), client.ForceOwnership))
	}
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(serviceEntryGVK.GroupVersion().WithKind("ServiceEntryList"))
	if err := r.Reader.List(ctx, &list, client.InNamespace(e.Namespace), client.MatchingLabels{LabelInstance: instance}); err != nil {
		return errors.Join(append(errs, err)...)
	}
	for i := range list.Items {
		if want[list.Items[i].GetName()] == nil {
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
		if err != nil || u.Hostname() == "" || isInternal(internal, u.Hostname()) {
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
	se := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"hosts":      toAny(hosts),
			"exportTo":   []any{".", callerNS},
			"location":   "MESH_EXTERNAL",
			"resolution": "DNS",
			"ports":      []any{map[string]any{"number": int64(443), "name": "tls", "protocol": "TLS"}},
		},
	}}
	se.SetGroupVersionKind(serviceEntryGVK)
	se.SetNamespace(e.Namespace)
	se.SetName("continuity-" + tier)
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

// cleanup puts the realm back to plain local login and removes what this
// instance created.
func (r *Reconciler) cleanup(ctx context.Context, ic *v1.IdentityContinuity, kc *keycloak.Client) error {
	if _, err := kc.SetRedirector(ctx, ic.Spec.Broker.Keycloak.BrowserFlow, ""); err != nil {
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
	return r.reconcileEgress(ctx, ic, ic.Spec.Egress, nil)
}
