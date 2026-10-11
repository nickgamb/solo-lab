package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/relationships"
)

const (
	condSynced = "Synced"
	// the store is removed with its model
	relationshipsFinalizer = "continuity.lab.solo.io/relationships"
	// how often a store is matched again without a change: what anyone
	// wrote to it directly is undone
	relationshipsEvery = 5 * time.Minute
)

// RelationshipReconciler keeps an OpenFGA store per RelationshipModel: its
// model, and exactly the Relationships that name it.
type RelationshipReconciler struct {
	client.Client
	FGA relationships.Store
	// deletes a model's store (relationships.Client.DeleteStore)
	DeleteStore func(ctx context.Context, storeID string) error
}

// StoreName is the OpenFGA store a model's relationships live in.
func StoreName(m *v1.RelationshipModel) string { return m.Namespace + "/" + m.Name }

func (r *RelationshipReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("relationships").
		For(&v1.RelationshipModel{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1.Relationship{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			rel := o.(*v1.Relationship)
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: rel.Namespace, Name: rel.Spec.Model}}}
		})).
		Complete(r)
}

func (r *RelationshipReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var m v1.RelationshipModel
	if err := r.Get(ctx, req.NamespacedName, &m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !m.DeletionTimestamp.IsZero() {
		if m.Status.StoreID != "" && r.DeleteStore != nil {
			if err := r.DeleteStore(ctx, m.Status.StoreID); err != nil {
				return ctrl.Result{}, fmt.Errorf("delete store %s: %w", m.Status.StoreID, err)
			}
		}
		controllerutil.RemoveFinalizer(&m, relationshipsFinalizer)
		return ctrl.Result{}, r.Update(ctx, &m)
	}
	if controllerutil.AddFinalizer(&m, relationshipsFinalizer) {
		if err := r.Update(ctx, &m); err != nil {
			return ctrl.Result{}, err
		}
	}

	var list v1.RelationshipList
	if err := r.List(ctx, &list, client.InNamespace(m.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	var declared []relationships.Declared
	for _, rel := range list.Items {
		if rel.Spec.Model == m.Name && rel.DeletionTimestamp.IsZero() {
			declared = append(declared, relationships.Declared{Name: rel.Name, Tuple: relationships.Tuple{
				User: rel.Spec.User, Relation: rel.Spec.Relation, Object: rel.Spec.Object}})
		}
	}

	res, err := relationships.Sync(ctx, r.FGA, StoreName(&m), m.Spec.Model, declared)
	base := m.DeepCopy()
	m.Status.ObservedGeneration = m.Generation
	if res.StoreID != "" {
		m.Status.StoreID = res.StoreID
	}
	if res.ModelID != "" {
		m.Status.ModelID = res.ModelID
	}
	cond := metav1.Condition{Type: condSynced, ObservedGeneration: m.Generation}
	switch {
	case err != nil:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "SyncFailed", truncate(err.Error(), maxMessage)
	case len(res.Rejected) > 0:
		names := make([]string, 0, len(res.Rejected))
		for n, why := range res.Rejected {
			names = append(names, n+": "+why)
		}
		slices.Sort(names)
		m.Status.Relationships = int32(res.Relationships)
		cond.Status, cond.Reason = metav1.ConditionFalse, "RelationshipsRefused"
		cond.Message = truncate(fmt.Sprintf("OpenFGA refused %d under the model: %s", len(names), strings.Join(names, "; ")), maxMessage)
	default:
		now := metav1.Now()
		m.Status.Relationships, m.Status.SyncedAt = int32(res.Relationships), &now
		cond.Status, cond.Reason = metav1.ConditionTrue, "Synced"
		cond.Message = fmt.Sprintf("%d relationships in store %s, model %s", res.Relationships, res.StoreID, res.ModelID)
	}
	meta.SetStatusCondition(&m.Status.Conditions, cond)
	if perr := r.Status().Patch(ctx, &m, client.MergeFrom(base)); perr != nil && !apierrors.IsNotFound(perr) {
		return ctrl.Result{}, perr
	}
	if err != nil {
		return ctrl.Result{}, err // retried with backoff
	}
	return ctrl.Result{RequeueAfter: relationshipsEvery}, nil
}
