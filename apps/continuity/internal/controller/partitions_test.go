package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
)

func partitionPolicy(ns, name, tier, action, target string) *unstructured.Unstructured {
	p := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"action": action}}}
	if target != "" {
		p.Object["spec"].(map[string]any)["targetRefs"] = []any{map[string]any{"group": "networking.istio.io", "kind": "ServiceEntry", "name": target}}
	}
	p.SetGroupVersionKind(authzPolicyGVK)
	p.SetNamespace(ns)
	p.SetName(name)
	p.SetLabels(map[string]string{LabelTier: tier})
	return p
}

// Only a DENY in the egress namespace on the tier's own ServiceEntry marks it
// partitioned: a policy anywhere else with the tier label says nothing about
// the instance's path.
func TestMarkPartitions(t *testing.T) {
	r := &Reconciler{Reader: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(
		partitionPolicy("sv-egress", "cut-auth0", "auth0", "DENY", "continuity-auth0"),
		partitionPolicy("alice", "spoof-okta", "okta", "DENY", "continuity-okta"),
		partitionPolicy("sv-egress", "allow-entra", "entra", "ALLOW", "continuity-entra"),
		partitionPolicy("sv-egress", "untargeted-ping", "ping", "DENY", ""),
	).Build()}
	ic := &v1.IdentityContinuity{Spec: v1.IdentityContinuitySpec{Egress: &v1.Egress{Namespace: "sv-egress"}}}
	for _, n := range []string{"auth0", "okta", "entra", "ping"} {
		ic.Status.Tiers = append(ic.Status.Tiers, v1.TierStatus{Name: n})
	}
	r.markPartitions(context.Background(), ic)
	want := map[string]bool{"auth0": true, "okta": false, "entra": false, "ping": false}
	for _, s := range ic.Status.Tiers {
		if s.Partitioned != want[s.Name] {
			t.Errorf("%s: partitioned=%v, want %v", s.Name, s.Partitioned, want[s.Name])
		}
	}
	ic.Spec.Egress = nil
	r.markPartitions(context.Background(), ic)
	for _, s := range ic.Status.Tiers {
		if s.Partitioned {
			t.Errorf("%s: partitioned with no egress", s.Name)
		}
	}
}
