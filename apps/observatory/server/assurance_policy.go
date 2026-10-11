package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/open-policy-agent/opa/v1/ast"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// The assurance decision logic, edited live: the Rego module in the
// ConfigMap labelled continuity.lab.solo.io/assurance-policy (key
// policy.rego), wherever it is: beside the gate on OSS, beside Solo's
// ext-auth service on Enterprise. Both runtimes load it from there. Read and
// written as the signed-in admin; a module is compiled here first and
// written only if it defines what both runtimes ask.
const (
	policyLabel = "continuity.lab.solo.io/assurance-policy"
	policyKey   = "policy.rego"
)

var gvrConfigMap = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

// the rules a module must define, and who asks for each
var policyRules = []struct{ name, why string }{
	{"decision", "the assurance gate asks data.assurance.decision"},
	{"extauth", "Solo's ext-auth service asks data.assurance.extauth[<rule>]"},
}

// LogicView: the module and where it is. Without a ConfigMap, the gate's
// own module, which can't be saved from here.
type LogicView struct {
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	Text            string `json:"text"`
	// the gate's word: the version it decides with, and why it refused a
	// newer one
	Running string `json:"running,omitempty"`
	Refused string `json:"refused,omitempty"`
	Note    string `json:"note,omitempty"`
}

// Marker: a compile error, by position (the editors' markers).
type Marker struct {
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	Message string `json:"message"`
}

// Policy (GET /api/assurance/{ns}/{name}/policy): the decision logic, from
// its ConfigMap, with what the chain's gate says it runs.
func (a *Assurance) Policy(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if a.instance(ns, name) == nil {
		http.Error(w, "no such IdentityContinuity", http.StatusNotFound)
		return
	}
	text, running, refused, gateErr := a.gatePolicy(r.Context(), ns)
	v := LogicView{Running: running, Refused: refused}
	cl, err := a.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	cm, err := policyConfigMap(r.Context(), cl, running)
	switch {
	case err != nil && gateErr != nil:
		http.Error(w, err.Error()+"; "+gateErr.Error(), http.StatusBadGateway)
		return
	case err != nil:
		v.Text, v.Note = text, err.Error()+": the gate's module, read only"
	default:
		v.Namespace, v.Name, v.ResourceVersion = cm.GetNamespace(), cm.GetName(), cm.GetResourceVersion()
		v.Text, _, _ = unstructured.NestedString(cm.Object, "data", policyKey)
	}
	writeJSON(w, v)
}

// PutPolicy (PUT /api/assurance/{ns}/{name}/policy): the decision logic,
// {"text", "resourceVersion"}, as the signed-in admin. 400 with markers
// ({"errors": [...]}) when it doesn't compile or lacks a rule either
// runtime asks; 409 when the ConfigMap changed since it was read.
func (a *Assurance) PutPolicy(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if a.instance(ns, name) == nil {
		http.Error(w, "no such IdentityContinuity", http.StatusNotFound)
		return
	}
	var in struct {
		Text            string `json:"text"`
		ResourceVersion string `json:"resourceVersion"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || in.ResourceVersion == "" {
		http.Error(w, `need {"text": ..., "resourceVersion": ...}`, http.StatusBadRequest)
		return
	}
	if ms := compilePolicy(in.Text); len(ms) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": ms})
		return
	}
	cl, err := a.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	_, running, _, _ := a.gatePolicy(r.Context(), ns)
	cm, err := policyConfigMap(r.Context(), cl, running)
	if err != nil {
		http.Error(w, err.Error()+" (make layer-47 creates it)", http.StatusConflict)
		return
	}
	cm.SetResourceVersion(in.ResourceVersion)
	if err := unstructured.SetNestedField(cm.Object, in.Text, "data", policyKey); err != nil {
		httpErr(w, err)
		return
	}
	out, err := cl.Resource(gvrConfigMap).Namespace(cm.GetNamespace()).Update(r.Context(), cm, metav1.UpdateOptions{FieldManager: fieldManager})
	if apierrors.IsConflict(err) {
		http.Error(w, "the decision logic changed since it was read: reload and edit again", http.StatusConflict)
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"namespace": out.GetNamespace(), "name": out.GetName(), "resourceVersion": out.GetResourceVersion()})
}

// gatePolicy: the module the chain's gate decides with, the ConfigMap
// version it names (namespace/name@resourceVersion, or built-in), and why
// it refused a newer one.
func (a *Assurance) gatePolicy(ctx context.Context, ns string) (text, running, refused string, err error) {
	g := a.gate(ns)
	if g == nil || g.EvaluatePort == 0 {
		return "", "", "", fmt.Errorf("no assurance gate in %s", ns)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.url("/v1/policy"), nil)
	resp, err := a.http.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("the assurance gate didn't answer: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("the assurance gate's policy answer: %s", resp.Status)
	}
	return string(body), resp.Header.Get("X-Assurance-Policy"), resp.Header.Get("X-Assurance-Policy-Error"), nil
}

// policyConfigMap: the labelled ConfigMap; of several, the one the gate
// names (running).
func policyConfigMap(ctx context.Context, cl dynamic.Interface, running string) (*unstructured.Unstructured, error) {
	l, err := cl.Resource(gvrConfigMap).List(ctx, metav1.ListOptions{LabelSelector: policyLabel + "=true"})
	if err != nil {
		return nil, err
	}
	switch len(l.Items) {
	case 0:
		return nil, fmt.Errorf("no ConfigMap labelled %s", policyLabel)
	case 1:
		return &l.Items[0], nil
	}
	var names []string
	for i, cm := range l.Items {
		ref := cm.GetNamespace() + "/" + cm.GetName()
		if strings.HasPrefix(running, ref+"@") {
			return &l.Items[i], nil
		}
		names = append(names, ref)
	}
	sort.Strings(names)
	return nil, fmt.Errorf("ConfigMaps %s are all labelled %s, and the gate names none of them", strings.Join(names, ", "), policyLabel)
}

// compilePolicy: why the module can't be saved, by position; none if it can.
func compilePolicy(text string) []Marker {
	m, err := ast.ParseModuleWithOpts(policyKey, text, ast.ParserOptions{RegoVersion: ast.RegoV1})
	if err != nil {
		return markers(err)
	}
	if m == nil {
		return []Marker{{Line: 1, Col: 1, Message: "the module is empty"}}
	}
	at := func(loc *ast.Location) (int, int) {
		if loc == nil {
			return 1, 1
		}
		return max(loc.Row, 1), max(loc.Col, 1)
	}
	if p := strings.TrimPrefix(m.Package.Path.String(), "data."); p != "assurance" {
		l, c := at(m.Package.Location)
		return []Marker{{Line: l, Col: c, Message: "package " + p + ": the decision logic is package assurance"}}
	}
	c := ast.NewCompiler()
	if c.Compile(map[string]*ast.Module{policyKey: m}); c.Failed() {
		return markers(c.Errors)
	}
	var out []Marker
	for _, want := range policyRules {
		if !defines(m, want.name) {
			l, c := at(m.Package.Location)
			out = append(out, Marker{Line: l, Col: c, Message: "no rule " + want.name + ": " + want.why})
		}
	}
	return out
}

func defines(m *ast.Module, name string) bool {
	for _, r := range m.Rules {
		if ref := r.Head.Ref(); len(ref) > 0 && ref[0].Value.Compare(ast.Var(name)) == 0 {
			return true
		}
	}
	return false
}

func markers(err error) []Marker {
	var errs ast.Errors
	if !errors.As(err, &errs) || len(errs) == 0 {
		return []Marker{{Line: 1, Col: 1, Message: err.Error()}}
	}
	out := make([]Marker, 0, len(errs))
	for _, e := range errs {
		m := Marker{Line: 1, Col: 1, Message: e.Message}
		if e.Location != nil {
			m.Line, m.Col = max(e.Location.Row, 1), max(e.Location.Col, 1)
		}
		out = append(out, m)
	}
	return out
}
