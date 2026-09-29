package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

// Resources is the Advanced editor: read any object as YAML, dry-run an
// edit, apply it. Writes run as the signed-in admin through impersonation.
type Resources struct {
	k     *Kube
	admin string // the group every signed-in user has (the auth middleware checks it)
}

const fieldManager = "observatory"

// The only identity the observatory may assume, pinned in rbac.yaml: one fixed
// user, the admin group, and the person as an extra (in the audit log's
// impersonatedUser.extra), so no token or bug can make it anyone else.
const (
	impersonatedUser = "observatory:admin"
	userExtra        = "observatory-user"
)

func (s *Resources) client(r *http.Request) (dynamic.Interface, error) {
	u := userFrom(r.Context())
	if u.Name == "" {
		return nil, errors.New("no user")
	}
	cfg := rest.CopyConfig(s.k.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: impersonatedUser,
		Groups:   []string{"observatory:" + s.admin},
		Extra:    map[string][]string{userExtra: {u.Name}},
	}
	return dynamic.NewForConfig(cfg)
}

func (s *Resources) resource(r *http.Request, apiVersion, kind, ns string) (dynamic.ResourceInterface, error) {
	gvr, ok := s.k.GVR(apiVersion, kind)
	if !ok {
		return nil, errors.New("unknown kind " + apiVersion + " " + kind)
	}
	c, err := s.client(r)
	if err != nil {
		return nil, err
	}
	if ns != "" {
		return c.Resource(gvr).Namespace(ns), nil
	}
	return c.Resource(gvr), nil
}

func (s *Resources) Get(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ri, err := s.resource(r, q.Get("apiVersion"), q.Get("kind"), q.Get("namespace"))
	if err != nil {
		httpErr(w, err)
		return
	}
	o, err := ri.Get(r.Context(), q.Get("name"), metav1.GetOptions{})
	if err != nil {
		httpErr(w, err)
		return
	}
	writeYAML(w, clean(o))
}

// Apply takes the edited YAML. ?dryRun=true returns what the API server
// would store, for the diff view; otherwise it's a server-side apply. The
// YAML's resourceVersion is kept, so an object changed since it was loaded is
// a conflict (409), and fields another manager owns are a conflict too.
// ?force=true (the editor's "apply anyway") drops both checks.
func (s *Resources) Apply(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		httpErr(w, err)
		return
	}
	var o unstructured.Unstructured
	if err := yaml.Unmarshal(body, &o.Object); err != nil {
		http.Error(w, "invalid YAML: "+err.Error(), http.StatusBadRequest)
		return
	}
	ri, err := s.resource(r, o.GetAPIVersion(), o.GetKind(), o.GetNamespace())
	if err != nil {
		httpErr(w, err)
		return
	}
	force := r.URL.Query().Get("force") == "true"
	o.SetManagedFields(nil)
	unstructured.RemoveNestedField(o.Object, "status")
	if force {
		unstructured.RemoveNestedField(o.Object, "metadata", "resourceVersion")
	}
	patch, _ := json.Marshal(o.Object)
	opts := metav1.PatchOptions{FieldManager: fieldManager, Force: ptr(force)}
	if r.URL.Query().Get("dryRun") == "true" {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	out, err := ri.Patch(r.Context(), o.GetName(), types.ApplyPatchType, patch, opts)
	if err != nil {
		httpErr(w, err)
		return
	}
	writeYAML(w, clean(out))
}

func clean(o *unstructured.Unstructured) *unstructured.Unstructured {
	o = o.DeepCopy()
	o.SetManagedFields(nil)
	a := o.GetAnnotations()
	delete(a, "kubectl.kubernetes.io/last-applied-configuration")
	if len(a) == 0 {
		a = nil
	}
	o.SetAnnotations(a)
	return o
}

func writeYAML(w http.ResponseWriter, o *unstructured.Unstructured) {
	b, err := yaml.Marshal(o.Object)
	if err != nil {
		httpErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Write(b)
}

func httpErr(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	msg := err.Error()
	var se interface{ Status() metav1.Status }
	if errors.As(err, &se) {
		st := se.Status()
		if st.Code > 0 {
			code = int(st.Code)
		}
		msg = st.Message
	}
	if strings.HasPrefix(msg, "unknown kind") {
		code = http.StatusBadRequest
	}
	http.Error(w, msg, code)
}

func ptr[T any](v T) *T { return &v }
