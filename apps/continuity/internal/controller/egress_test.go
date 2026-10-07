package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/keycloak"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/probe"
)

func TestExternalHosts(t *testing.T) {
	internal := []string{"lab", "svc", "cluster.local"}
	d := &probe.Discovery{TokenEndpoint: "https://gamb.us.auth0.com/oauth/token", JWKSURI: "https://keys.example.com/jwks", UserinfoEndpoint: "https://gamb.us.auth0.com/userinfo"}
	if got := externalHosts(internal, "https://gamb.us.auth0.com/", d); !slices.Equal(got, []string{"gamb.us.auth0.com", "keys.example.com"}) {
		t.Errorf("auth0: %v", got)
	}
	if got := externalHosts(internal, "http://keycloak.sv-workforce.svc/admin/realms/workforce", nil); len(got) != 0 {
		t.Errorf("a cluster Service needs no ServiceEntry: %v", got)
	}
	if got := externalHosts(internal, "https://idp.ledgerline.lab/realms/ledgerline", nil); len(got) != 0 {
		t.Errorf("in-lab issuer needs no ServiceEntry: %v", got)
	}
	if got := externalHosts(internal, "https://notlab.com/", nil); !slices.Equal(got, []string{"notlab.com"}) {
		t.Errorf("suffix must match on a label boundary: %v", got)
	}
}

// The ServiceEntry reaches the broker's namespace and every spec.egress.exportTo
// namespace, so their calls to the upstream leave through the same waypoint.
func TestServiceEntryExportTo(t *testing.T) {
	e := &v1.Egress{Namespace: "sv-egress", Waypoint: "egress-waypoint", ExportTo: []string{"agentgateway-system"}}
	se := serviceEntry(e, "sv-identity", "sv-identity.sterling-vance", "gluu", []string{"gluu.example"})
	got, _, _ := unstructured.NestedSlice(se.Object, "spec", "exportTo")
	want := []any{".", "sv-identity", "agentgateway-system"}
	if len(got) != len(want) {
		t.Fatalf("exportTo = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("exportTo = %v, want %v", got, want)
		}
	}
	// each namespace once; the egress namespace is "."
	e.ExportTo = []string{"sv-egress", "agentgateway-system", "sv-identity", "agentgateway-system"}
	se = serviceEntry(e, "sv-egress", "sv-egress.sterling-vance", "gluu", []string{"gluu.example"})
	got, _, _ = unstructured.NestedSlice(se.Object, "spec", "exportTo")
	if fmt.Sprint(got) != "[. agentgateway-system sv-identity]" {
		t.Fatalf("exportTo = %v", got)
	}
}

// Deleting with the broker down still removes the ServiceEntries, and lets
// the object go once the grace period is over.
func TestFinalizeWithBrokerDown(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	kc := keycloak.New(down.URL, "r")
	kc.SetCredentials("id", "secret")

	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ic := &v1.IdentityContinuity{ObjectMeta: metav1.ObjectMeta{Namespace: "sv-identity", Name: "sterling-vance",
		Finalizers: []string{finalizer}, DeletionTimestamp: &metav1.Time{Time: time.Now().Add(-cleanupGrace - time.Minute)}},
		Spec: v1.IdentityContinuitySpec{Egress: &v1.Egress{Namespace: "sv-egress"}}}
	mine := serviceEntry(ic.Spec.Egress, ic.Namespace, instanceOf(ic), "gluu", []string{"gluu.example"})
	theirs := serviceEntry(ic.Spec.Egress, "other", "other.x", "okta", []string{"okta.example"})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ic, mine, theirs).Build()
	r := &Reconciler{Client: c, Reader: c, Recorder: events.NewFakeRecorder(10), brokers: map[types.NamespacedName]*keycloak.Client{},
		discovery: map[string]*probe.Discovery{}, profiles: map[types.NamespacedName]profileMark{}}

	var cur v1.IdentityContinuity
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ic), &cur); err != nil {
		t.Fatal(err)
	}
	if _, err := r.finalize(context.Background(), &cur, kc, nil); err != nil {
		t.Fatal(err)
	}
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(serviceEntryGVK.GroupVersion().WithKind("ServiceEntryList"))
	if err := c.List(context.Background(), &list, client.InNamespace("sv-egress")); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].GetName() != "continuity-okta" {
		t.Fatalf("ServiceEntries left: %d (want only another instance's)", len(list.Items))
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ic), &cur); !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer kept after the grace period: %v %v", err, cur.Finalizers)
	}
}

// Within the grace period, the ServiceEntries go at once; the object waits for Keycloak.
func TestFinalizeWithinGraceRetries(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	kc := keycloak.New(down.URL, "r")
	kc.SetCredentials("id", "secret")
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	ic := &v1.IdentityContinuity{ObjectMeta: metav1.ObjectMeta{Namespace: "sv-identity", Name: "sterling-vance",
		Finalizers: []string{finalizer}, DeletionTimestamp: &metav1.Time{Time: time.Now()}},
		Spec: v1.IdentityContinuitySpec{Egress: &v1.Egress{Namespace: "sv-egress"}}}
	mine := serviceEntry(ic.Spec.Egress, ic.Namespace, instanceOf(ic), "gluu", []string{"gluu.example"})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ic, mine).Build()
	r := &Reconciler{Client: c, Reader: c, Recorder: events.NewFakeRecorder(10)}
	res, err := r.finalize(context.Background(), ic, kc, nil)
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("result %+v err %v: want a retry", res, err)
	}
	var se unstructured.Unstructured
	se.SetGroupVersionKind(serviceEntryGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "sv-egress", Name: "continuity-gluu"}, &se); !apierrors.IsNotFound(err) {
		t.Fatalf("ServiceEntry kept while Keycloak is down: %v", err)
	}
}
