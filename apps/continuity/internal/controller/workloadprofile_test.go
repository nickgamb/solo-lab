package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/keycloak"
)

// A profile follows its chain: Available on its first eligible IdP,
// FailedClosed on one that can't meet it, and Ready False when a client's
// tokens can't carry the upstream's assurance.
func TestWorkloadProfileFollowsTheChain(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 60})
		case strings.HasSuffix(r.URL.Path, "/clients") && r.URL.Query().Get("clientId") == "kagent":
			json.NewEncoder(w).Encode([]map[string]any{{"clientId": "kagent", "redirectUris": []string{"https://kagent.sterling.lab/oauth2/redirect"},
				"defaultClientScopes": []string{"profile", AssuranceScope},
				"protocolMappers":     []map[string]any{{"protocolMapper": "oidc-audience-mapper", "config": map[string]string{"included.client.audience": "ai-gateway"}}}}})
		case strings.HasSuffix(r.URL.Path, "/clients") && r.URL.Query().Get("clientId") == "legacy":
			json.NewEncoder(w).Encode([]map[string]any{{"clientId": "legacy", "defaultClientScopes": []string{"profile"}}})
		case strings.HasSuffix(r.URL.Path, "/clients"):
			w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer broker.Close()

	ic := &v1.IdentityContinuity{ObjectMeta: metav1.ObjectMeta{Name: "sterling-vance", Namespace: "sv-identity"},
		Spec: v1.IdentityContinuitySpec{Broker: v1.Broker{Keycloak: v1.KeycloakBroker{URL: broker.URL, Realm: "r", CredentialsRef: v1.LocalRef{Name: "creds"}}},
			Tiers: []v1.Tier{
				{Name: "keycloak", Type: "oidc", Assurance: &v1.TierAssurance{Levels: []v1.AssuranceLevel{{ACR: "aal2", Level: "AAL2"}}}},
				{Name: "contingency", Type: "oidc", Assurance: &v1.TierAssurance{Default: "AAL1"}},
				{Name: "break-glass", Type: "local"}}},
		Status: v1.IdentityContinuityStatus{Active: "keycloak"}}
	p := &v1.WorkloadProfile{ObjectMeta: metav1.ObjectMeta{Name: "advisor-workspace", Namespace: "sv-identity", Generation: 1},
		Spec: v1.WorkloadProfileSpec{Continuity: "sterling-vance", Criticality: "Critical", Clients: []string{"kagent"},
			Assurance: v1.ProfileAssurance{Minimum: "AAL2"}}}
	creds := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "sv-identity"},
		Data: map[string][]byte{"client-id": []byte("id"), "client-secret": []byte("s")}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ic, p, creds).
		WithStatusSubresource(&v1.WorkloadProfile{}, &v1.IdentityContinuity{}).Build()
	rec := events.NewFakeRecorder(10)
	cont := &Reconciler{Client: c, Reader: c, Recorder: rec, brokers: map[types.NamespacedName]*keycloak.Client{}}
	r := &WorkloadProfileReconciler{Client: c, Continuity: cont, clients: map[types.NamespacedName]clientsMark{}}
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "sv-identity", Name: "advisor-workspace"}
	get := func() *v1.WorkloadProfile {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		var got v1.WorkloadProfile
		if err := c.Get(ctx, key, &got); err != nil {
			t.Fatal(err)
		}
		return &got
	}

	got := get()
	if got.Status.Phase != "Available" || got.Status.Serving != "keycloak" || got.Status.ServingLevel != "AAL2" ||
		strings.Join(got.Status.EligibleIdPs, ",") != "keycloak" {
		t.Fatalf("on keycloak: %+v", got.Status)
	}
	if cl := got.Status.Clients; len(cl) != 1 || !cl[0].Found || !cl[0].AssuranceScope || cl[0].Audiences[0] != "ai-gateway" {
		t.Fatalf("clients %+v", cl)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, "Ready") {
		t.Fatalf("not Ready: %+v", got.Status.Conditions)
	}

	ic.Status.Active = "contingency"
	if err := c.Status().Update(ctx, ic); err != nil {
		t.Fatal(err)
	}
	if got = get(); got.Status.Phase != "FailedClosed" || got.Status.ServingLevel != "AAL1" {
		t.Fatalf("on contingency: %+v", got.Status)
	}
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "Warning FailedClosed") || !strings.Contains(e, "can't meet AAL2") {
			t.Errorf("event %q", e)
		}
	default:
		t.Error("no FailedClosed event")
	}

	got.Spec.Clients = []string{"legacy", "gone"}
	got.Generation = 2
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got = get()
	ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if ready.Status != metav1.ConditionFalse || ready.Reason != "ClientWithoutAssurance" || !strings.Contains(ready.Message, "legacy") {
		t.Fatalf("a client without the assurance scope: %+v", ready)
	}
}
