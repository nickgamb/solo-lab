package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/routing"
)

const condRouted = "Routed"

// reconcileRouting writes the fabric's routing policy into the gateway
// policy spec.routing names, and its outcome into status.routing: each
// rule's IdP now, from the chain's health and what the broker has set up
// (keep). Sign-ins no rule matches go to the active tier. When the write
// fails, the gateway keeps the last policy written, and status keeps saying
// what that was.
func (r *Reconciler) reconcileRouting(ctx context.Context, ic *v1.IdentityContinuity, st map[string]*v1.TierStatus, keep map[string]bool) {
	rt := ic.Spec.Routing
	if rt == nil {
		ic.Status.Routing = nil
		meta.RemoveStatusCondition(&ic.Status.Conditions, condRouted)
		return
	}
	routes := routing.Resolve(ic.Spec, st, func(n string) bool { return keep[n] })
	fallback := ""
	for _, t := range ic.Spec.Tiers {
		if t.Name == ic.Status.Active && t.Type == "oidc" && keep[t.Name] {
			fallback = t.Name
		}
	}
	pol := &unstructured.Unstructured{Object: map[string]any{}}
	pol.SetAPIVersion(rt.Policy.APIVersion)
	pol.SetKind(rt.Policy.Kind)
	pol.SetNamespace(rt.Policy.Namespace)
	pol.SetName(rt.Policy.Name)
	set := []any{map[string]any{"name": ":path", "value": routing.Path(ic.Spec, routes, fallback)}}
	if err := unstructured.SetNestedSlice(pol.Object, set, "spec", "traffic", "transformation", "request", "set"); err != nil {
		r.setRouted(ic, metav1.ConditionFalse, "PolicyNotWritten", err.Error())
		return
	}
	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(pol), client.FieldOwner("continuity-controller"), client.ForceOwnership); err != nil {
		r.setRouted(ic, metav1.ConditionFalse, "PolicyNotWritten",
			fmt.Sprintf("%s %s/%s: %v", rt.Policy.Kind, rt.Policy.Namespace, rt.Policy.Name, err))
		return
	}
	was := map[string]string{}
	for _, s := range ic.Status.Routing {
		was[s.Name] = s.IdP
	}
	parts := make([]string, 0, len(routes)+1)
	for _, s := range routes {
		to := s.IdP
		if to == "" {
			to = orNone(fallback)
		}
		parts = append(parts, s.Name+" → "+to)
		if old, ok := was[s.Name]; ok && old != s.IdP {
			typ := corev1.EventTypeNormal
			if s.IdP == "" || s.Reason != "its first IdP" {
				typ = corev1.EventTypeWarning
			}
			r.Recorder.Eventf(ic, nil, typ, "RouteChanged", "Route", "sign-ins for %s now go to %s (%s)", s.Name, orNone(s.IdP), s.Reason)
		}
	}
	parts = append(parts, "everything else → "+orNone(fallback))
	ic.Status.Routing = routes
	r.setRouted(ic, metav1.ConditionTrue, "Applied", strings.Join(parts, "; "))
}

func (r *Reconciler) setRouted(ic *v1.IdentityContinuity, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&ic.Status.Conditions, metav1.Condition{Type: condRouted, Status: status, Reason: reason,
		Message: truncate(msg, maxMessage), ObservedGeneration: ic.Generation})
}
