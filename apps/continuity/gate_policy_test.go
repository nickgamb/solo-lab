package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nickgamb/solo-lab/apps/continuity/internal/assurance"
)

// The decision logic from its ConfigMap, live: an edit that compiles
// decides the next request; one that doesn't is refused, the last good
// module kept deciding, and the refusal served beside it.
func TestPolicyReload(t *testing.T) {
	t.Cleanup(func() { assurance.Use(nil) })
	now := time.Now()
	g := testGate(t, "keycloak", now, now)
	var cm *corev1.ConfigMap
	set := func(rv, text string) {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: policyConfigMap, Namespace: "sv-identity", ResourceVersion: rv},
			Data: map[string]string{policyKey: text}}
	}
	g.policy = &policyWatch{ns: "sv-identity", name: policyConfigMap, get: func(context.Context) (*corev1.ConfigMap, error) {
		if cm == nil {
			return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, policyConfigMap)
		}
		return cm, nil
	}}
	ctx := context.Background()
	stranger := &assurance.Session{IdP: "elsewhere"}
	reason := func() string {
		_, d := g.judge(ctx, "", "", broker, stranger)
		return d.Reason
	}
	policy := func() (string, string, string) {
		w := httptest.NewRecorder()
		g.evaluateHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/policy", nil))
		return w.Body.String(), w.Header().Get(policyHeader), w.Header().Get(policyErrorHeader)
	}

	// none yet: the built-in module
	g.policy.poll(ctx)
	if r := reason(); r != "session from elsewhere, which isn't in the chain" {
		t.Fatalf("built-in: %q", r)
	}
	if text, src, _ := policy(); src != "built-in" || text != assurance.Policy {
		t.Fatalf("built-in served as %q", src)
	}

	edited := strings.Replace(assurance.Policy,
		"decision := decide(input.rules, input.chain, input.current, input.session, input.now)",
		`decision := object.union(decide(input.rules, input.chain, input.current, input.session, input.now), {"reason": "edited live"})`, 1)
	if edited == assurance.Policy {
		t.Fatal("the decision rule moved: update the edit")
	}
	set("10", edited)
	g.policy.poll(ctx)
	if r := reason(); r != "edited live" {
		t.Fatalf("edit not deciding: %q", r)
	}
	if text, src, refused := policy(); src != "sv-identity/assurance-policy@10" || refused != "" || text != edited {
		t.Fatalf("served %q %q", src, refused)
	}

	// refused: no decision rule, then a syntax error; the edit keeps deciding
	for i, bad := range []string{
		strings.Replace(edited, "decision := ", "decision_off := ", 1),
		edited + "\ndecision := {\n",
		"package elsewhere\n\ndecision := {}\n",
	} {
		rv := string(rune('a' + i))
		set(rv, bad)
		g.policy.poll(ctx)
		if r := reason(); r != "edited live" {
			t.Fatalf("bad module %d took effect: %q", i, r)
		}
		if _, src, refused := policy(); src != "sv-identity/assurance-policy@10" || !strings.Contains(refused, "resourceVersion "+rv+" refused") {
			t.Fatalf("bad module %d: served %q %q", i, src, refused)
		}
	}

	// gone: the last good module stays
	cm = nil
	g.policy.poll(ctx)
	if r := reason(); r != "edited live" {
		t.Fatalf("ConfigMap gone: %q", r)
	}

	// back to the shipped module
	set("11", assurance.Policy)
	g.policy.poll(ctx)
	if r := reason(); r != "session from elsewhere, which isn't in the chain" {
		t.Fatalf("restored: %q", r)
	}
	if _, src, refused := policy(); src != "sv-identity/assurance-policy@11" || refused != "" {
		t.Fatalf("restored: served %q %q", src, refused)
	}
}
