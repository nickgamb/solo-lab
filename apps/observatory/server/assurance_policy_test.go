package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

const goodPolicy = `package assurance

import rego.v1

decision := {"allow": true, "reason": "ok"}

extauth[name] := {"allow": true} if some name in {"notes"}
`

func policyCM(ns, rv, text string) *unstructured.Unstructured {
	o := object("v1", "ConfigMap", ns, "assurance-policy", "data", map[string]any{policyKey: text})
	md := o["metadata"].(map[string]any)
	md["resourceVersion"], md["labels"] = rv, map[string]any{policyLabel: "true"}
	return &unstructured.Unstructured{Object: o}
}

// The decision logic from its labelled ConfigMap, with what the gate says
// it runs; saved only when it compiles and defines both runtimes' rules,
// with markers where it doesn't, and refused when it changed since read.
func TestAssurancePolicyEdit(t *testing.T) {
	gate := object("v1", "Service", "id", "decisions", "spec", map[string]any{
		"ports": []any{map[string]any{"name": "evaluate", "port": int64(9002)}}})
	gate["metadata"].(map[string]any)["labels"] = map[string]any{gateLabel: "true"}
	ic := object("continuity.lab.solo.io/v1alpha1", "IdentityContinuity", "id", "firm")
	running := "agw/assurance-policy@7"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/policy" {
			http.Error(w, "unexpected "+r.URL.String(), http.StatusNotFound)
			return
		}
		w.Header().Set("X-Assurance-Policy", running)
		w.Write([]byte("package assurance # the gate's\n"))
	}))
	defer srv.Close()
	lists := map[schema.GroupVersionResource]string{gvrConfigMap: "ConfigMapList"}
	cl := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, policyCM("agw", "7", goodPolicy))
	// the API server's optimistic concurrency, which the fake leaves out
	cl.PrependReactor("update", "configmaps", func(act k8stesting.Action) (bool, runtime.Object, error) {
		u := act.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		cur, err := cl.Tracker().Get(gvrConfigMap, u.GetNamespace(), u.GetName())
		if m, _ := meta.Accessor(cur); err == nil && m.GetResourceVersion() != u.GetResourceVersion() {
			return true, nil, apierrors.NewConflict(gvrConfigMap.GroupResource(), u.GetName(), nil)
		}
		return false, nil, nil
	})
	a := &Assurance{k: storeKube(map[string][]map[string]any{"identitycontinuities": {ic}, "services": {gate}}),
		http: srv.Client(), gateURL: srv.URL, writer: func(*http.Request) (dynamic.Interface, error) { return cl, nil }}
	call := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/", strings.NewReader(body))
		r.SetPathValue("ns", "id")
		r.SetPathValue("name", "firm")
		w := httptest.NewRecorder()
		if method == http.MethodGet {
			a.Policy(w, r)
		} else {
			a.PutPolicy(w, r)
		}
		return w
	}
	put := func(rv, text string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"text": text, "resourceVersion": rv})
		return call(http.MethodPut, string(b))
	}

	w := call(http.MethodGet, "")
	var v LogicView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if v.Namespace != "agw" || v.Name != "assurance-policy" || v.ResourceVersion != "7" || v.Text != goodPolicy || v.Running != running {
		t.Fatalf("view %+v", v)
	}

	for _, c := range []struct {
		text string
		line int
		want string
	}{
		{strings.Replace(goodPolicy, `"reason": "ok"}`, `"reason": "ok"`, 1), 7, ""},
		{strings.Replace(goodPolicy, "extauth[name]", "other[name]", 1), 1, "no rule extauth"},
		{strings.Replace(goodPolicy, "decision :=", "verdict :=", 1), 1, "no rule decision"},
		{strings.Replace(goodPolicy, "package assurance", "package elsewhere", 1), 1, "package elsewhere"},
		{goodPolicy + "\nnope := unknown_fn(1)\n", 9, "unknown_fn"},
	} {
		w := put("7", c.text)
		var out struct{ Errors []Marker }
		if w.Code != http.StatusBadRequest || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Errors) == 0 {
			t.Fatalf("%q: %d %s", c.want, w.Code, w.Body)
		}
		if e := out.Errors[0]; e.Line != c.line || !strings.Contains(e.Message, c.want) {
			t.Fatalf("%q: marker %+v", c.want, out.Errors)
		}
	}
	if got, _ := cl.Resource(gvrConfigMap).Namespace("agw").Get(context.Background(), "assurance-policy", metav1.GetOptions{}); str(got.Object, "data", policyKey) != goodPolicy {
		t.Fatal("a refused module was written")
	}

	edited := goodPolicy + "\n# edited\n"
	if w := put("7", edited); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	got, _ := cl.Resource(gvrConfigMap).Namespace("agw").Get(context.Background(), "assurance-policy", metav1.GetOptions{})
	if str(got.Object, "data", policyKey) != edited || got.GetLabels()[policyLabel] != "true" {
		t.Fatalf("saved %v", got.Object)
	}
	if w := put("6", goodPolicy); w.Code != http.StatusConflict {
		t.Fatalf("stale: %d %s", w.Code, w.Body)
	}

	// of two labelled, the one the gate names
	cl = fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, policyCM("agw", "7", goodPolicy), policyCM("id", "3", "package assurance\n"))
	running = "id/assurance-policy@3"
	if w := call(http.MethodGet, ""); json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Namespace != "id" {
		t.Fatalf("two: %s", w.Body)
	}
	// none: the gate's module, read only
	cl, v = fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists), LogicView{}
	if w := call(http.MethodGet, ""); json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Name != "" || v.Note == "" || !strings.Contains(v.Text, "the gate's") {
		t.Fatalf("none: %s", w.Body)
	}
	if w := put("7", goodPolicy); w.Code != http.StatusConflict {
		t.Fatalf("none, save: %d %s", w.Code, w.Body)
	}
}
