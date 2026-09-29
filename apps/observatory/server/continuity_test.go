package main

import (
	"context"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

func TestGrantSecretAddsNameOnce(t *testing.T) {
	role := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
		"metadata": map[string]any{"name": "continuity-controller-secrets", "namespace": "sv-identity",
			"labels": map[string]any{secretsRoleLabel: "true"}},
		"rules": []any{
			map[string]any{"apiGroups": []any{""}, "resources": []any{"secrets"}, "resourceNames": []any{"continuity-controller"}, "verbs": []any{"get"}},
			map[string]any{"apiGroups": []any{""}, "resources": []any{"configmaps"}, "verbs": []any{"get"}},
		},
	}}
	cl := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvrRole: "RoleList"}, role)
	for i := 0; i < 2; i++ {
		if err := grantSecret(context.Background(), cl, "sv-identity", "upstream-okta"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := cl.Resource(gvrRole).Namespace("sv-identity").Get(context.Background(), "continuity-controller-secrets", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rules, _, _ := unstructured.NestedSlice(got.Object, "rules")
	names := toStrings(rules[0].(map[string]any)["resourceNames"])
	if !slices.Equal(names, []string{"continuity-controller", "upstream-okta"}) {
		t.Errorf("secret rule names: %v", names)
	}
	if _, ok := rules[1].(map[string]any)["resourceNames"]; ok {
		t.Error("touched a rule that isn't about secrets")
	}
}
