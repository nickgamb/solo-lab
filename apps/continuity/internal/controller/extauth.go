package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/assurance"
)

// Solo's ext-auth service enforces the assurance rules on Enterprise: it
// loads the decision logic (ConfigMap assurance-policy, the install's: the
// Rego the gate runs) and, beside it, this instance's state as a Rego data
// module (ConfigMap assurance-state-<chain>, written here): every rule as it
// applies, the chain's tiers, the IdPs signing people in now, and the
// broker's issuer and keys, so the token is verified there too. One
// AuthConfig per rule (assurance-<rule>; the chain's default rule,
// assurance-<chain>-default) asks the logic for that rule.
const (
	condExtAuth = "AssuranceDelivered"
	// how often the state is written even when nothing changed: the Rego
	// takes a state older than extAuthStale as not knowing which IdPs sign
	// people in now
	extAuthHeartbeat = 20 * time.Second
	extAuthStale     = 60 * time.Second
	jwksEvery        = 10 * time.Minute
	policyModule     = "assurance-policy"
)

var authConfigList = schema.GroupVersionKind{Group: "extauth.solo.io", Version: "v1", Kind: "AuthConfigList"}

type extAuthMark struct {
	hash   string
	at     time.Time
	jwks   string
	jwksAt time.Time
}

// stateModule is the ConfigMap a chain's state lives in.
func stateModule(ic *v1.IdentityContinuity) string { return "assurance-state-" + ic.Name }

// DefaultRuleName is the ext-auth rule name of a chain's default rule.
func DefaultRuleName(chain string) string { return chain + "-default" }

func (r *Reconciler) reconcileExtAuth(ctx context.Context, ic *v1.IdentityContinuity) {
	ns := r.ExtAuthNamespace
	if ns == "" {
		meta.RemoveStatusCondition(&ic.Status.Conditions, condExtAuth)
		return
	}
	fail := func(format string, a ...any) {
		meta.SetStatusCondition(&ic.Status.Conditions, metav1.Condition{Type: condExtAuth, Status: metav1.ConditionFalse,
			Reason: "NotDelivered", Message: truncate(fmt.Sprintf(format, a...), maxMessage), ObservedGeneration: ic.Generation})
	}
	if ic.Status.Broker == nil || ic.Status.Broker.Issuer == "" {
		fail("the broker's issuer isn't known yet")
		return
	}
	var ps v1.WorkloadProfileList
	if err := r.List(ctx, &ps, client.InNamespace(ic.Namespace)); err != nil {
		fail("workload profiles: %v", err)
		return
	}
	rules := map[string]any{
		DefaultRuleName(ic.Name): map[string]any{"mode": "Enforce", "rules": assurance.RulesInput(assurance.Effective(v1.WorkloadProfileSpec{}, ic.Spec.AssurancePolicy))},
	}
	names := []string{DefaultRuleName(ic.Name)}
	for _, p := range ps.Items {
		if p.Spec.Continuity != ic.Name {
			continue
		}
		mode := p.Spec.Mode
		if mode == "" {
			mode = "Enforce"
		}
		rules[p.Name] = map[string]any{"mode": mode, "rules": assurance.RulesInput(assurance.Effective(p.Spec, ic.Spec.AssurancePolicy))}
		names = append(names, p.Name)
	}
	key := types.NamespacedName{Namespace: ic.Namespace, Name: ic.Name}
	r.mu.Lock()
	if r.extauth == nil {
		r.extauth = map[types.NamespacedName]extAuthMark{}
	}
	mark := r.extauth[key]
	r.mu.Unlock()
	if mark.jwks == "" || time.Since(mark.jwksAt) > jwksEvery {
		k, err := brokerJWKS(ctx, ic)
		if err != nil && mark.jwks == "" {
			fail("the broker's keys: %v", err)
			return
		}
		if err == nil {
			mark.jwks, mark.jwksAt = k, time.Now()
		}
	}
	state := map[string]any{
		"issuer": ic.Status.Broker.Issuer, "jwks": mark.jwks, "stale_s": int(extAuthStale.Seconds()),
		"chain": assurance.ChainInput(assurance.EnabledTiers(ic.Spec.Tiers)), "current": assurance.CurrentInput(assurance.CurrentOf(ic)),
		"rules": rules,
	}
	body, _ := json.Marshal(state)
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	if hash != mark.hash || time.Since(mark.at) > extAuthHeartbeat {
		now := time.Now()
		state["generated_at"] = now.Unix()
		body, _ = json.Marshal(state)
		cm := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": stateModule(ic), "namespace": ns, "labels": map[string]any{LabelInstance: instanceOf(ic)}},
			"data":     map[string]any{"state.rego": "package assurance_state\n\n# written by the continuity controller: " + instanceOf(ic) + "\nstate := " + string(body) + "\n"},
		}}
		if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(cm), client.FieldOwner("continuity-controller"), client.ForceOwnership); err != nil {
			fail("ConfigMap %s/%s: %v", ns, stateModule(ic), err)
			return
		}
		mark.hash, mark.at = hash, now
	}
	r.mu.Lock()
	r.extauth[key] = mark
	r.mu.Unlock()

	// one AuthConfig per rule; the instance's others go
	want := map[string]bool{}
	for _, n := range names {
		name := "assurance-" + n
		want[name] = true
		ac := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "extauth.solo.io/v1", "kind": "AuthConfig",
			"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{LabelInstance: instanceOf(ic)}},
			"spec": map[string]any{"configs": []any{map[string]any{"opaAuth": map[string]any{
				"modules": []any{map[string]any{"name": policyModule, "namespace": ns}, map[string]any{"name": stateModule(ic), "namespace": ns}},
				"query":   fmt.Sprintf("data.assurance.extauth[%q]", n),
			}}}},
		}}
		if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(ac), client.FieldOwner("continuity-controller"), client.ForceOwnership); err != nil {
			fail("AuthConfig %s/%s: %v", ns, name, err)
			return
		}
	}
	var have unstructured.UnstructuredList
	have.SetGroupVersionKind(authConfigList)
	if err := r.Reader.List(ctx, &have, client.InNamespace(ns), client.MatchingLabels{LabelInstance: instanceOf(ic)}); err == nil {
		for i := range have.Items {
			if !want[have.Items[i].GetName()] {
				_ = r.Delete(ctx, &have.Items[i])
			}
		}
	}
	meta.SetStatusCondition(&ic.Status.Conditions, metav1.Condition{Type: condExtAuth, Status: metav1.ConditionTrue, Reason: "Delivered",
		Message: fmt.Sprintf("%d rules for Solo's ext-auth service (%s): %s", len(names), ns, strings.Join(names, ", ")), ObservedGeneration: ic.Generation})
}

// brokerJWKS: the broker realm's keys, as JSON, for the Rego to verify
// tokens with.
func brokerJWKS(ctx context.Context, ic *v1.IdentityContinuity) (string, error) {
	u := strings.TrimSuffix(ic.Spec.Broker.Keycloak.URL, "/") + "/realms/" + ic.Spec.Broker.Keycloak.Realm + "/protocol/openid-connect/certs"
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK || !json.Valid(body) {
		return "", fmt.Errorf("%s answered %d", u, resp.StatusCode)
	}
	return string(body), nil
}
