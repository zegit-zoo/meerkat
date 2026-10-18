// Package controller holds the meerkat operator's reconcilers
// (meerkat-mob#10, phase 2).
//
// This slice has the KnowledgeBase reconciler only, and it renders
// nothing yet: it checks each KnowledgeBase's place in the tree after
// admission and reports it in status. The MeerkatMob reconciler, which
// renders Deployments and content-source.yaml from a checked tree,
// builds on the TreeValid condition it sets.
package controller

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
	"github.com/zegit-zoo/meerkat/internal/operator/webhook"
)

// The reconciler reads KnowledgeBases and writes their status.
//
// +kubebuilder:rbac:groups=meerkat-mob.dev,resources=knowledgebases,verbs=get;list;watch
// +kubebuilder:rbac:groups=meerkat-mob.dev,resources=knowledgebases/status,verbs=get;update;patch

// KnowledgeBaseReconciler re-runs the webhook's tree rules on every
// KnowledgeBase and records the result in status: observedGeneration,
// depth and the TreeValid condition.
type KnowledgeBaseReconciler struct {
	client.Client
}

// Reconcile checks one KnowledgeBase against every KnowledgeBase in its
// namespace as they are now.
func (r *KnowledgeBaseReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var kb v1alpha1.KnowledgeBase
	if err := r.Get(ctx, req.NamespacedName, &kb); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !kb.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	var list v1alpha1.KnowledgeBaseList
	if err := r.List(ctx, &list, client.InNamespace(kb.Namespace)); err != nil {
		return reconcile.Result{}, fmt.Errorf("listing KnowledgeBases in %q: %w", kb.Namespace, err)
	}

	status := *kb.Status.DeepCopy()
	status.ObservedGeneration = kb.Generation
	status.Depth = webhook.Depth(kb.Name, list.Items)
	meta.SetStatusCondition(&status.Conditions, treeCondition(&kb, list.Items))
	if equality.Semantic.DeepEqual(status, kb.Status) {
		return reconcile.Result{}, nil
	}
	kb.Status = status
	// Update, not a patch: a write that raced this one fails on the
	// resourceVersion and is retried against the newer object.
	if err := r.Status().Update(ctx, &kb); err != nil {
		return reconcile.Result{}, fmt.Errorf("updating KnowledgeBase %s status: %w", req.NamespacedName, err)
	}
	return reconcile.Result{}, nil
}

// treeCondition is the TreeValid condition for kb in the tree that
// existing forms.
func treeCondition(kb *v1alpha1.KnowledgeBase, existing []v1alpha1.KnowledgeBase) metav1.Condition {
	c := metav1.Condition{Type: v1alpha1.ConditionTreeValid, ObservedGeneration: kb.Generation}
	warnings, err := webhook.CheckTree(kb, existing)
	switch {
	case err != nil:
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, v1alpha1.ReasonTreeRefused, err.Error()
	case len(warnings) > 0:
		c.Status, c.Reason, c.Message = metav1.ConditionUnknown, v1alpha1.ReasonParentMissing, strings.Join(warnings, "; ")
	default:
		c.Status, c.Reason, c.Message = metav1.ConditionTrue, v1alpha1.ReasonTreeValid, "the parent chain ends at a root and the subtree is within the depth limits"
	}
	return c
}

// SetupWithManager registers the reconciler. A KnowledgeBase's
// condition depends on the others in its namespace (its ancestors, and
// everything below it), so a spec change, create or delete of any of
// them re-queues all of them. Status-only writes do not change the
// generation, so the reconciler's own updates do not re-trigger it.
func (r *KnowledgeBaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.KnowledgeBase{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1alpha1.KnowledgeBase{},
			handler.EnqueueRequestsFromMapFunc(r.namespacePeers),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("knowledgebase").
		Complete(r)
}

// namespacePeers lists every KnowledgeBase in obj's namespace other
// than obj.
func (r *KnowledgeBaseReconciler) namespacePeers(ctx context.Context, obj client.Object) []reconcile.Request {
	var list v1alpha1.KnowledgeBaseList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing KnowledgeBases to re-queue", "namespace", obj.GetNamespace())
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Name == obj.GetName() {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: list.Items[i].Name}})
	}
	return out
}
