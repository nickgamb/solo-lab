package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/assurance"
)

// AssuranceScope is the broker client scope that puts the upstream's
// assurance in a client's tokens.
const AssuranceScope = "continuity-assurance"

// clientsEvery: how often a profile's clients are read again from the
// broker without a change to the profile.
const clientsEvery = time.Minute

// WorkloadProfileReconciler keeps each WorkloadProfile's status: its phase
// on the chain as it serves now, the IdPs whose sign-ins can meet it, and
// its clients as the broker has them registered. The assurance gate enforces
// the profile itself; this is what operators and the Observatory read.
type WorkloadProfileReconciler struct {
	client.Client
	// The continuity controller: the broker's admin client and the recorder.
	Continuity *Reconciler

	mu      sync.Mutex
	clients map[types.NamespacedName]clientsMark
}

type clientsMark struct {
	at         time.Time
	generation int64
}

func (r *WorkloadProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.clients = map[types.NamespacedName]clientsMark{}
	return ctrl.NewControllerManagedBy(mgr).Named("workloadprofile").
		For(&v1.WorkloadProfile{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// a profile follows its chain: the active tier and the spec, not
		// every probe's status write
		Watches(&v1.IdentityContinuity{}, handler.EnqueueRequestsFromMapFunc(r.profilesOf), builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				o, n := e.ObjectOld.(*v1.IdentityContinuity), e.ObjectNew.(*v1.IdentityContinuity)
				return o.Generation != n.Generation || o.Status.Active != n.Status.Active
			},
		})).
		Complete(r)
}

// profilesOf: the profiles that follow an IdentityContinuity.
func (r *WorkloadProfileReconciler) profilesOf(ctx context.Context, o client.Object) []reconcile.Request {
	var list v1.WorkloadProfileList
	if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, p := range list.Items {
		if p.Spec.Continuity == o.GetName() {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&p)})
		}
	}
	return out
}

func (r *WorkloadProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p v1.WorkloadProfile
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		if apierrors.IsNotFound(err) {
			r.mu.Lock()
			delete(r.clients, req.NamespacedName)
			r.mu.Unlock()
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := p.DeepCopy()
	ready := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled", Message: "the chain and the profile's clients are known"}

	var ic v1.IdentityContinuity
	err := r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Spec.Continuity}, &ic)
	switch {
	case apierrors.IsNotFound(err):
		p.Status.Phase, p.Status.Serving, p.Status.ServingLevel, p.Status.EligibleIdPs = "FailedClosed", "", "", nil
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, "ContinuityNotFound", fmt.Sprintf("no IdentityContinuity %s in %s", p.Spec.Continuity, p.Namespace)
	case err != nil:
		return ctrl.Result{}, err
	default:
		chain := enabled(ic.Spec.Tiers)
		active := ic.Status.Active
		p.Status.Serving = active
		p.Status.ServingLevel = ""
		for _, t := range chain {
			if t.Name == active {
				p.Status.ServingLevel = assurance.Ceiling(t).String()
			}
		}
		rules := assurance.Effective(p.Spec, ic.Spec.AssurancePolicy)
		p.Status.EligibleIdPs = assurance.Eligible(rules, chain)
		phase := assurance.Phase(rules, chain, active)
		if phase != p.Status.Phase && p.Status.Phase != "" {
			typ, msg := corev1.EventTypeNormal, fmt.Sprintf("%s: sign-ins through %s can meet it", phase, orNone(active))
			switch {
			case phase == "FailedClosed" && (p.Spec.Mode == "" || p.Spec.Mode == "Enforce"):
				typ = corev1.EventTypeWarning
				msg = fmt.Sprintf("FailedClosed: sign-ins through %s (at most %s) can't meet %s; requests are refused", orNone(active), orNone(p.Status.ServingLevel), rules.Minimum)
			case phase == "FailedClosed":
				msg = fmt.Sprintf("FailedClosed: sign-ins through %s (at most %s) can't meet %s; mode %s, so requests go through", orNone(active), orNone(p.Status.ServingLevel), rules.Minimum, p.Spec.Mode)
			}
			r.Continuity.Recorder.Eventf(&p, nil, typ, phase, "Evaluate", "%s", msg)
		}
		p.Status.Phase = phase
		var names []string
		for _, t := range chain {
			names = append(names, t.Name)
		}
		var unknown []string
		for _, n := range rules.AllowedIdPs {
			if !slices.Contains(names, n) {
				unknown = append(unknown, n)
			}
		}
		if len(unknown) > 0 {
			ready.Status, ready.Reason = metav1.ConditionFalse, "UnknownIdP"
			ready.Message = fmt.Sprintf("allowedIdPs names %s, not in %s's chain", strings.Join(unknown, ", "), ic.Name)
		}
		if err := r.readClients(ctx, &p, &ic); err != nil {
			ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, "BrokerUnavailable", truncate("reading clients: "+err.Error(), maxMessage)
		} else if ready.Status == metav1.ConditionTrue {
			for _, c := range p.Status.Clients {
				switch {
				case !c.Found:
					ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, "ClientNotFound", fmt.Sprintf("the broker has no client %s", c.ClientID)
				case !c.AssuranceScope && rules.Minimum > 1:
					ready.Status, ready.Reason = metav1.ConditionFalse, "ClientWithoutAssurance"
					ready.Message = fmt.Sprintf("%s's tokens don't carry the upstream's assurance (client scope %s), so no request through it can prove %s", c.ClientID, AssuranceScope, rules.Minimum)
				default:
					continue
				}
				break
			}
		}
	}
	ready.ObservedGeneration = p.Generation
	meta.SetStatusCondition(&p.Status.Conditions, ready)
	p.Status.ObservedGeneration = p.Generation
	if err := r.Status().Patch(ctx, &p, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: clientsEvery}, nil
}

// readClients reads the profile's clients from the broker when due: the
// profile changed, or clientsEvery passed.
func (r *WorkloadProfileReconciler) readClients(ctx context.Context, p *v1.WorkloadProfile, ic *v1.IdentityContinuity) error {
	key := client.ObjectKeyFromObject(p)
	r.mu.Lock()
	m, ok := r.clients[key]
	r.mu.Unlock()
	if ok && m.generation == p.Generation && time.Since(m.at) < clientsEvery && len(p.Status.Clients) == len(p.Spec.Clients) {
		return nil
	}
	if len(p.Spec.Clients) == 0 {
		p.Status.Clients = nil
		return nil
	}
	kc, err := r.Continuity.broker(ctx, ic)
	if err != nil {
		return err
	}
	out := make([]v1.ClientRegistration, 0, len(p.Spec.Clients))
	for _, id := range p.Spec.Clients {
		reg, err := kc.ClientRegistration(ctx, id)
		if err != nil {
			return err
		}
		out = append(out, v1.ClientRegistration{ClientID: id, Found: reg.Found, RedirectURIs: reg.RedirectURIs,
			Audiences: reg.Audiences, AssuranceScope: slices.Contains(reg.DefaultScopes, AssuranceScope)})
	}
	p.Status.Clients = out
	r.mu.Lock()
	r.clients[key] = clientsMark{at: time.Now(), generation: p.Generation}
	r.mu.Unlock()
	return nil
}

// enabled: the chain's tiers that can ever be active.
func enabled(ts []v1.Tier) []v1.Tier {
	var out []v1.Tier
	for _, t := range ts {
		if t.Enabled == nil || *t.Enabled {
			out = append(out, t)
		}
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
