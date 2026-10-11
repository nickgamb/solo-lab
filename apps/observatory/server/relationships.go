package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/openfga/language/pkg/go/transformer"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// Relationships serves the Relationships window of the Identity Continuity
// tab, for one RelationshipModel: its OpenFGA model as declared and the
// Relationships that name it, from the watch cache. The continuity
// controller keeps OpenFGA matching them; nothing here calls OpenFGA. The
// what-if is the relationship guard's own answer (its evaluate API). Writes
// go through as the signed-in admin.
type Relationships struct {
	k    *Kube
	res  *Resources
	http *http.Client
	// guardURL: where to reach the guard's evaluate port instead of its
	// Service (local development, through a port-forward)
	guardURL string
	// writer: the client writes go through, as the signed-in admin
	writer func(*http.Request) (dynamic.Interface, error)
}

func (rs *Relationships) client(r *http.Request) (dynamic.Interface, error) {
	if rs.writer != nil {
		return rs.writer(r)
	}
	return rs.res.client(r)
}

var (
	gvrRel      = schema.GroupVersionResource{Group: "continuity.lab.solo.io", Version: "v1alpha1", Resource: "relationships"}
	gvrRelModel = schema.GroupVersionResource{Group: "continuity.lab.solo.io", Version: "v1alpha1", Resource: "relationshipmodels"}
)

// guardLabel marks a Service as the relationship guard, whatever it's named:
// its port evaluate is what the Observatory asks.
const guardLabel = "continuity.lab.solo.io/relationship-guard"

// The Relationship CRD's patterns and lengths.
var (
	relUserRe     = regexp.MustCompile(`^[a-z_][a-z0-9_]*:[^\s#:]+(#[a-z_][a-z0-9_]*)?$`)
	relRelationRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	relObjectRe   = regexp.MustCompile(`^[a-z_][a-z0-9_]*:[^\s#:]+$`)
)

type RelationshipsView struct {
	Namespace       string             `json:"namespace"`
	Name            string             `json:"name"`
	ResourceVersion string             `json:"resourceVersion"`
	Description     string             `json:"description,omitempty"`
	Model           string             `json:"model"`
	Status          map[string]any     `json:"status,omitempty"`
	Relationships   []RelationshipItem `json:"relationships"`
	Guard           *ServiceRef        `json:"guard,omitempty"`
}

type RelationshipItem struct {
	Name            string `json:"name"`
	User            string `json:"user"`
	Relation        string `json:"relation"`
	Object          string `json:"object"`
	ResourceVersion string `json:"resourceVersion"`
}

type ServiceRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	port      int64
	override  string
}

func (g *ServiceRef) url(path string) string {
	if g.override != "" {
		return strings.TrimSuffix(g.override, "/") + path
	}
	return fmt.Sprintf("http://%s.%s.svc:%d%s", g.Name, g.Namespace, g.port, path)
}

func (rs *Relationships) model(ns, name string) *unstructured.Unstructured {
	for _, m := range rs.k.List("relationshipmodels") {
		if m.GetNamespace() == ns && m.GetName() == name {
			return m
		}
	}
	return nil
}

// of: the model's Relationships.
func (rs *Relationships) of(ns, model string) []RelationshipItem {
	out := []RelationshipItem{}
	for _, r := range rs.k.List("relationships") {
		if r.GetNamespace() == ns && str(r.Object, "spec", "model") == model {
			out = append(out, RelationshipItem{Name: r.GetName(), User: str(r.Object, "spec", "user"), Relation: str(r.Object, "spec", "relation"),
				Object: str(r.Object, "spec", "object"), ResourceVersion: r.GetResourceVersion()})
		}
	}
	return out
}

// guard: the relationship guard's Service, by its evaluate port.
func (rs *Relationships) guard() *ServiceRef {
	for _, s := range rs.k.List("services") {
		if _, ok := s.GetLabels()[guardLabel]; !ok {
			continue
		}
		for _, p := range slice(s.Object, "spec", "ports") {
			pm, _ := p.(map[string]any)
			if port, _ := pm["port"].(int64); str(pm, "name") == "evaluate" && port != 0 {
				return &ServiceRef{Namespace: s.GetNamespace(), Name: s.GetName(), port: port, override: rs.guardURL}
			}
		}
	}
	return nil
}

// Get (GET /api/relationships/{ns}/{name}): one RelationshipModel and its
// Relationships.
func (rs *Relationships) Get(w http.ResponseWriter, r *http.Request) {
	m := rs.model(r.PathValue("ns"), r.PathValue("name"))
	if m == nil {
		http.Error(w, "no such RelationshipModel", http.StatusNotFound)
		return
	}
	writeJSON(w, RelationshipsView{Namespace: m.GetNamespace(), Name: m.GetName(), ResourceVersion: m.GetResourceVersion(), Description: str(m.Object, "spec", "description"),
		Model: str(m.Object, "spec", "model"), Status: obj(m.Object, "status"), Relationships: rs.of(m.GetNamespace(), m.GetName()), Guard: rs.guard()})
}

// CodeMarker: where a document is wrong, 1-based, as the web's editor marks it.
type CodeMarker struct {
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	Message string `json:"message"`
}

var dslErrRe = regexp.MustCompile(`^syntax error at line=(\d+), column=(\d+): (.*)$`)

// modelErrors: where OpenFGA's own parser finds the DSL wrong (it reports
// 0-based positions, in its errors' text). OpenFGA checks the rest when the
// controller writes the model: the model's status says.
func modelErrors(dsl string) (out []CodeMarker) {
	if strings.TrimSpace(dsl) == "" {
		return []CodeMarker{{Line: 1, Col: 1, Message: "the model is empty"}}
	}
	defer func() {
		if p := recover(); p != nil {
			out = []CodeMarker{{Line: 1, Col: 1, Message: fmt.Sprint("the model doesn't parse: ", p)}}
		}
	}()
	_, err := transformer.TransformDSLToJSON(dsl)
	if err == nil {
		return nil
	}
	errs := []error{err}
	var multi *transformer.OpenFgaDslSyntaxMultipleError
	if errors.As(err, &multi) {
		errs = multi.Errors
	}
	for _, e := range errs {
		if m := dslErrRe.FindStringSubmatch(e.Error()); m != nil {
			line, _ := strconv.Atoi(m[1])
			col, _ := strconv.Atoi(m[2])
			out = append(out, CodeMarker{Line: line + 1, Col: col + 1, Message: m[3]})
		} else {
			out = append(out, CodeMarker{Line: 1, Col: 1, Message: e.Error()})
		}
	}
	return out
}

// SaveModel (PUT /api/relationships/{ns}/{name}/model): the model's DSL,
// {"model", "resourceVersion"}, as the signed-in admin, once OpenFGA's own
// parser reads it (400 with {"errors": [...]} where it doesn't); 409 if the
// model changed since it was read. The controller writes it to OpenFGA.
func (rs *Relationships) SaveModel(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var in struct {
		Model           string `json:"model"`
		ResourceVersion string `json:"resourceVersion"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, `need {"model": ..., "resourceVersion": ...}`, http.StatusBadRequest)
		return
	}
	if rs.model(ns, name) == nil {
		http.Error(w, "no such RelationshipModel", http.StatusNotFound)
		return
	}
	if errs := modelErrors(in.Model); len(errs) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": errs})
		return
	}
	cl, err := rs.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	md := map[string]any{}
	if in.ResourceVersion != "" {
		md["resourceVersion"] = in.ResourceVersion
	}
	patch, _ := json.Marshal(map[string]any{"metadata": md, "spec": map[string]any{"model": in.Model}})
	out, err := cl.Resource(gvrRelModel).Namespace(ns).Patch(r.Context(), name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: fieldManager})
	if apierrors.IsConflict(err) {
		http.Error(w, name+" changed since it was read: reload", http.StatusConflict)
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"resourceVersion": out.GetResourceVersion()})
}

// Add (POST /api/relationships/{ns}/{name}/relationships): a Relationship
// in the model, {"user", "relation", "object"}, as the signed-in admin,
// named after it (409 if the model already has it).
func (rs *Relationships) Add(w http.ResponseWriter, r *http.Request) {
	ns, model := r.PathValue("ns"), r.PathValue("name")
	var in struct {
		User     string `json:"user"`
		Relation string `json:"relation"`
		Object   string `json:"object"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(w, `need {"user": ..., "relation": ..., "object": ...}`, http.StatusBadRequest)
		return
	}
	switch {
	case len(in.User) > 256 || !relUserRe.MatchString(in.User):
		http.Error(w, "user: type:id, or type:id#relation for a set of users", http.StatusBadRequest)
		return
	case len(in.Relation) > 50 || !relRelationRe.MatchString(in.Relation):
		http.Error(w, "relation: lowercase letters, digits and _", http.StatusBadRequest)
		return
	case len(in.Object) > 256 || !relObjectRe.MatchString(in.Object):
		http.Error(w, "object: type:id", http.StatusBadRequest)
		return
	}
	if rs.model(ns, model) == nil {
		http.Error(w, "no such RelationshipModel", http.StatusNotFound)
		return
	}
	for _, x := range rs.of(ns, model) {
		if x.User == in.User && x.Relation == in.Relation && x.Object == in.Object {
			http.Error(w, "the model already has this relationship ("+x.Name+")", http.StatusConflict)
			return
		}
	}
	taken := map[string]bool{}
	for _, x := range rs.k.List("relationships") {
		if x.GetNamespace() == ns {
			taken[x.GetName()] = true
		}
	}
	cl, err := rs.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "continuity.lab.solo.io/v1alpha1", "kind": "Relationship",
		"metadata": map[string]any{"name": relationshipName(in.User, in.Relation, in.Object, taken), "namespace": ns},
		"spec":     map[string]any{"model": model, "user": in.User, "relation": in.Relation, "object": in.Object}}}
	out, err := cl.Resource(gvrRel).Namespace(ns).Create(r.Context(), u, metav1.CreateOptions{FieldManager: fieldManager})
	if apierrors.IsAlreadyExists(err) {
		http.Error(w, "a relationship named "+u.GetName()+" was added since this was read: reload", http.StatusConflict)
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, RelationshipItem{Name: out.GetName(), User: in.User, Relation: in.Relation, Object: in.Object, ResourceVersion: out.GetResourceVersion()})
}

// relationshipName: a Kubernetes name read from the relationship (its
// user's and object's ids around the relation), not one of taken.
func relationshipName(user, relation, object string, taken map[string]bool) string {
	id := func(s string) string {
		_, s, _ = strings.Cut(s, ":")
		s, _, _ = strings.Cut(s, "#")
		return s
	}
	base := dnsName(id(user) + "-" + relation + "-" + id(object))
	if base == "" {
		base = "relationship"
	}
	name := base
	for i := 2; taken[name]; i++ {
		suffix := "-" + strconv.Itoa(i)
		name = strings.TrimRight(base[:min(len(base), 63-len(suffix))], "-") + suffix
	}
	return name
}

// Remove (DELETE /api/relationships/{ns}/{name}/relationships/{rel}): one
// of the model's Relationships, as the signed-in admin; with the
// resourceVersion it was read at ({"resourceVersion": ...}), 409 if it
// changed since.
func (rs *Relationships) Remove(w http.ResponseWriter, r *http.Request) {
	ns, model, name := r.PathValue("ns"), r.PathValue("name"), r.PathValue("rel")
	if !nameRe.MatchString(ns) || !nameRe.MatchString(name) {
		http.Error(w, "need namespace and relationship name (DNS labels)", http.StatusBadRequest)
		return
	}
	var in struct {
		ResourceVersion string `json:"resourceVersion"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&in); err != nil && err != io.EOF {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	for _, x := range rs.k.List("relationships") {
		if x.GetNamespace() == ns && x.GetName() == name && str(x.Object, "spec", "model") != model {
			http.Error(w, name+" isn't one of "+model+"'s relationships", http.StatusNotFound)
			return
		}
	}
	cl, err := rs.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	opts := metav1.DeleteOptions{}
	if in.ResourceVersion != "" {
		opts.Preconditions = &metav1.Preconditions{ResourceVersion: &in.ResourceVersion}
	}
	err = cl.Resource(gvrRel).Namespace(ns).Delete(r.Context(), name, opts)
	if apierrors.IsConflict(err) {
		http.Error(w, name+" changed since it was read: reload", http.StatusConflict)
		return
	}
	if err != nil && !apierrors.IsNotFound(err) {
		httpErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Evaluate (POST /api/relationships/{ns}/{name}/evaluate): what the
// relationship guard decides for a caller, {"user", "groups", "agent",
// "server"}, as its evaluate API answers it.
func (rs *Relationships) Evaluate(w http.ResponseWriter, r *http.Request) {
	if rs.model(r.PathValue("ns"), r.PathValue("name")) == nil {
		http.Error(w, "no such RelationshipModel", http.StatusNotFound)
		return
	}
	g := rs.guard()
	if g == nil {
		http.Error(w, fmt.Sprintf("no relationship guard: a Service labelled %s with a port named evaluate", guardLabel), http.StatusServiceUnavailable)
		return
	}
	var in struct {
		User   string   `json:"user"`
		Groups []string `json:"groups"`
		Agent  string   `json:"agent"`
		Server string   `json:"server"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	b, _ := json.Marshal(in)
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, g.url("/v1/evaluate"), strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := rs.http.Do(req)
	if err != nil {
		http.Error(w, "the relationship guard didn't answer: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}
