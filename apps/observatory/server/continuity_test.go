package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
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
		if err := grantSecret(context.Background(), cl, "sv-identity", "upstream-okta", secretsRoleLabel); err != nil {
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

// A directory's credentials go to the profile sync's Role only, never the
// controller's.
func TestGrantSecretByPurpose(t *testing.T) {
	role := func(name, label string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
			"metadata": map[string]any{"name": name, "namespace": "sv-identity", "labels": map[string]any{label: "true"}},
			"rules":    []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"secrets"}, "resourceNames": []any{name}, "verbs": []any{"get"}}},
		}}
	}
	cl := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{gvrRole: "RoleList"},
		role("continuity-controller-secrets", secretsRoleLabel), role("continuity-sync-secrets", syncSecretsRoleLabel))
	if err := grantSecret(context.Background(), cl, "sv-identity", "directory-gluu", syncSecretsRoleLabel); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"continuity-controller-secrets": false, "continuity-sync-secrets": true} {
		got, _ := cl.Resource(gvrRole).Namespace("sv-identity").Get(context.Background(), name, metav1.GetOptions{})
		rules, _, _ := unstructured.NestedSlice(got.Object, "rules")
		if has := slices.Contains(toStrings(rules[0].(map[string]any)["resourceNames"]), "directory-gluu"); has != want {
			t.Errorf("%s grants directory-gluu: %v, want %v", name, has, want)
		}
	}
}

// Two transitions in the same second both reach the feed; one already
// reported never does again.
func TestReportSameSecondTransitions(t *testing.T) {
	item := func(trs ...map[string]any) ContinuityView {
		l := []any{}
		for _, tr := range trs {
			l = append(l, tr)
		}
		return ContinuityView{Items: []map[string]any{{"metadata": map[string]any{"namespace": "sv-identity", "name": "sterling-vance"},
			"status": map[string]any{"transitions": l}}}}
	}
	tr := func(at, from, to string) map[string]any {
		return map[string]any{"time": at, "from": from, "to": to, "reason": "FailoverActivated"}
	}
	s := NewTrafficStore(16, NewHub(), func() *Index { return &Index{} })
	c := &Continuity{}
	old := tr("2026-10-06T10:00:00Z", "", "gluu")
	c.Report(item(old), s, &Index{}) // history from before: not news
	a, b := tr("2026-10-06T10:05:00Z", "gluu", "auth0"), tr("2026-10-06T10:05:00Z", "auth0", "keycloak")
	c.Report(item(old, a), s, &Index{})
	c.Report(item(old, a, b), s, &Index{})
	c.Report(item(old, a, b), s, &Index{})
	got := s.Recent("", 10)
	if len(got) != 2 || got[0].Attrs["to"] != "keycloak" || got[1].Attrs["to"] != "auth0" {
		t.Fatalf("feed %+v: each transition once", got)
	}
}

// A sync Job is only made from this instance's own CronJob, under a
// server-generated name.
func TestStartSyncJobChecksTheCronJob(t *testing.T) {
	cron := func(name, instance, sa string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "batch/v1", "kind": "CronJob",
			"metadata": map[string]any{"name": name, "namespace": "sv-identity", "labels": map[string]any{instanceLabel: instance}},
			"spec": map[string]any{"jobTemplate": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": "continuity-sync"}},
				"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"serviceAccountName": sa}}}}},
		}}
	}
	cl := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{gvrCron: "CronJobList", gvrJob: "JobList"},
		cron("sterling-vance-profile-sync", "sv-identity.sterling-vance", "continuity-sync"), cron("other-profile-sync", "sv-identity.other", "continuity-sync"),
		cron("rogue-profile-sync", "sv-identity.sterling-vance", "cluster-admin"))
	created := 0
	cl.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		o := a.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		if o.GetName() == "" {
			o.SetName(o.GetGenerateName() + fmt.Sprint(created))
		}
		created++
		return false, nil, nil
	})
	ic := func(cj string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "sterling-vance", "namespace": "sv-identity"},
			"status": map[string]any{"sync": map[string]any{"cronJob": cj}}}}
	}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if _, code, err := startSyncJob(r, cl, "sv-identity", ic("other-profile-sync"), "manual", nil); err == nil || code != http.StatusConflict || created != 0 {
		t.Fatalf("another instance's CronJob: code %d err %v created %d", code, err, created)
	}
	if _, code, err := startSyncJob(r, cl, "sv-identity", ic("rogue-profile-sync"), "manual", nil); err == nil || code != http.StatusConflict || created != 0 {
		t.Fatalf("a template not running as continuity-sync: code %d err %v created %d", code, err, created)
	}
	name, _, err := startSyncJob(r, cl, "sv-identity", ic("sterling-vance-profile-sync"), "manual", nil)
	if err != nil || !strings.HasPrefix(name, "sterling-vance-profile-sync-manual-") {
		t.Fatalf("job %q err %v", name, err)
	}
	if _, err := cl.Resource(gvrJob).Namespace("sv-identity").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Fatalf("job %s not created: %v", name, err)
	}
}

func TestPutSpecChecksNames(t *testing.T) {
	c := &Continuity{}
	for _, path := range []string{"/api/continuity/sv-identity/Bad_Name", "/api/continuity/..%2Fx/sterling-vance"} {
		mux := http.NewServeMux()
		mux.HandleFunc("PUT /api/continuity/{ns}/{name}", c.PutSpec)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{}`)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", path, w.Code)
		}
	}
}

// fakeAPI is the slice of the Kubernetes API the continuity routes use: one
// IdentityContinuity (resourceVersion checked on update), Secrets applied,
// and the sync's Role.
type fakeAPI struct {
	mu      sync.Mutex
	ic      map[string]any
	rv      int
	secrets []string
	// who wrote what: the impersonated user (empty: the server's own account)
	secretAs, roleAs []string
	labelled         []bool // each Secret written carries the credentials label
}

func (f *fakeAPI) serve(t *testing.T) *Continuity {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		const ics = "/apis/continuity.lab.solo.io/v1alpha1/namespaces/sv-identity/identitycontinuities"
		switch {
		case r.URL.Path == ics && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"apiVersion": "continuity.lab.solo.io/v1alpha1", "kind": "IdentityContinuityList", "items": []any{f.ic}})
		case r.URL.Path == ics+"/sterling-vance" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(f.ic)
		case r.URL.Path == ics+"/sterling-vance" && r.Method == http.MethodPut:
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			if in["metadata"].(map[string]any)["resourceVersion"] != fmt.Sprint(f.rv) {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "Conflict", "code": 409,
					"message": "the object has been modified"})
				return
			}
			f.rv++
			in["metadata"].(map[string]any)["resourceVersion"] = fmt.Sprint(f.rv)
			f.ic = in
			json.NewEncoder(w).Encode(in)
		case strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/sv-identity/secrets/") && r.Method == http.MethodPatch:
			f.secrets = append(f.secrets, strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/sv-identity/secrets/"))
			f.secretAs = append(f.secretAs, r.Header.Get("Impersonate-User"))
			b, _ := io.ReadAll(r.Body)
			f.labelled = append(f.labelled, strings.Contains(string(b), `"`+credentialsLabel+`":"true"`))
			w.Write(b)
		case strings.HasSuffix(r.URL.Path, "/roles") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleList", "items": []any{map[string]any{
				"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": map[string]any{"name": "r", "namespace": "sv-identity"},
				"rules": []any{map[string]any{"resources": []any{"secrets"}, "resourceNames": []any{"x"}, "verbs": []any{"get"}}}}}})
		case strings.Contains(r.URL.Path, "/roles/") && r.Method == http.MethodPut:
			f.roleAs = append(f.roleAs, r.Header.Get("Impersonate-User"))
			io.Copy(w, r.Body)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := &rest.Config{Host: srv.URL}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	k := &Kube{cfg: cfg, dyn: dyn}
	return &Continuity{k: k, res: &Resources{k: k, admin: "admins"}}
}

func call(h http.HandlerFunc, pattern, method, path, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc(method+" "+pattern, h)
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), userKey{}, User{Name: "nick"}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// The spec is replaced, not merged: a tier left out is gone. A stale
// resourceVersion is a 409.
func TestPutSpecReplacesAndDetectsConflicts(t *testing.T) {
	f := &fakeAPI{rv: 7, ic: map[string]any{"apiVersion": "continuity.lab.solo.io/v1alpha1", "kind": "IdentityContinuity",
		"metadata": map[string]any{"name": "sterling-vance", "namespace": "sv-identity", "resourceVersion": "7"},
		"spec":     map[string]any{"tiers": []any{map[string]any{"name": "gluu"}, map[string]any{"name": "auth0"}}}}}
	c := f.serve(t)
	put := func(rv string) *httptest.ResponseRecorder {
		return call(c.PutSpec, "/api/continuity/{ns}/{name}", http.MethodPut, "/api/continuity/sv-identity/sterling-vance",
			`{"resourceVersion":"`+rv+`","spec":{"tiers":[{"name":"gluu"}]}}`)
	}
	if w := put("7"); w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	if tiers := slice(f.ic, "spec", "tiers"); len(tiers) != 1 {
		t.Fatalf("tiers %v: the removed tier must go", tiers)
	}
	if w := put("7"); w.Code != http.StatusConflict || w.Body.Len() > 100 {
		t.Fatalf("stale resourceVersion: %d %q, want 409 and a short text", w.Code, w.Body)
	}
	if w := call(c.PutSpec, "/api/continuity/{ns}/{name}", http.MethodPut, "/api/continuity/sv-identity/sterling-vance", `{"tiers":[]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a bare spec: %d, want 400", w.Code)
	}
}

// A Secret is written only under a name the instance refers to for that use.
func TestPutSecretOnlyForReferencedNames(t *testing.T) {
	f := &fakeAPI{ic: map[string]any{"apiVersion": "continuity.lab.solo.io/v1alpha1", "kind": "IdentityContinuity",
		"metadata": map[string]any{"name": "sterling-vance", "namespace": "sv-identity"},
		"spec": map[string]any{
			"tiers": []any{map[string]any{"name": "gluu", "oidc": map[string]any{"clientSecretRef": map[string]any{"name": "upstream-gluu"}},
				"directory": map[string]any{"credentialsRef": map[string]any{"name": "directory-gluu"}}}},
			"sync": map[string]any{"suspend": true}}}}
	c := f.serve(t)
	put := func(name, body string) int {
		return call(c.PutSecret, "/api/continuity/{ns}/secrets/{name}", http.MethodPut, "/api/continuity/sv-identity/secrets/"+name, body).Code
	}
	tier := `{"value":"s3cret","for":"tier"}`
	dir := `{"data":{"client-id":"c","client-secret":"s"},"for":"directory"}`
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"upstream-gluu", tier, http.StatusOK},
		{"directory-gluu", dir, http.StatusOK},
		{"continuity-sync", dir, http.StatusOK}, // spec.sync's default credentialsRef
		{"directory-gluu", tier, http.StatusConflict},
		{"upstream-gluu", dir, http.StatusConflict},
		{"keycloak-admin", tier, http.StatusConflict},
		{"Bad_Name", tier, http.StatusBadRequest},
		{"upstream-gluu", `{"name":"other","value":"s"}`, http.StatusBadRequest},
	} {
		if got := put(c.name, c.body); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.name, c.body, got, c.want)
		}
	}
	if !slices.Equal(f.secretAs, []string{impersonatedUser, impersonatedUser, impersonatedUser}) || slices.Contains(f.labelled, false) {
		t.Fatalf("secrets written as %v, labelled %v: as the signed-in admin, each with the credentials label", f.secretAs, f.labelled)
	}
	if len(f.roleAs) == 0 || slices.ContainsFunc(f.roleAs, func(u string) bool { return u != "" }) {
		t.Fatalf("the sync's Role updated as %v: as the Observatory's own account, not the admin", f.roleAs)
	}
	if !slices.Equal(f.secrets, []string{"upstream-gluu", "directory-gluu", "continuity-sync"}) {
		t.Fatalf("secrets written %v", f.secrets)
	}
}

// The editor applies only the object it opened.
func TestApplyRefusesAnotherObject(t *testing.T) {
	gvrCM := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	res := &Resources{k: &Kube{cfg: &rest.Config{Host: "http://unused"}, kinds: map[schema.GroupVersionResource]string{gvrCM: "ConfigMap"}}, admin: "admins"}
	const opened = "/api/resource?apiVersion=v1&kind=ConfigMap&namespace=sv-identity&name=a"
	for name, doc := range map[string]string{
		"another name":      "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: b, namespace: sv-identity}\n",
		"another namespace": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a, namespace: kube-system}\n",
		"another kind":      "apiVersion: v1\nkind: Secret\nmetadata: {name: a, namespace: sv-identity}\n",
		"not YAML":          "{",
	} {
		if w := call(res.Apply, "/api/resource", http.MethodPost, opened+"&dryRun=true", doc); w.Code != http.StatusBadRequest && w.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want refused", name, w.Code)
		}
	}
	if w := call(res.Apply, "/api/resource", http.MethodPost, "/api/resource", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a, namespace: sv-identity}\n"); w.Code != http.StatusBadRequest {
		t.Errorf("no object named in the query: %d, want 400", w.Code)
	}
}

func TestEditorRefusesSecrets(t *testing.T) {
	k := &Kube{cfg: &rest.Config{Host: "http://unused"}, kinds: map[schema.GroupVersionResource]string{gvrSec: "Secret"}}
	res := &Resources{k: k, admin: "admins"}
	for _, h := range []http.HandlerFunc{res.Get, res.Apply} {
		w := call(h, "/api/resource", http.MethodPost, "/api/resource?apiVersion=v1&kind=Secret&namespace=sv-identity&name=x",
			"apiVersion: v1\nkind: Secret\nmetadata: {name: x, namespace: sv-identity}\n")
		if w.Code != http.StatusForbidden {
			t.Errorf("%d, want 403", w.Code)
		}
	}
}
