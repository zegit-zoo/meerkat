package controller

import (
	"context"
	"fmt"
	"math"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
	"github.com/zegit-zoo/meerkat/internal/operator/imagepolicy"
	"github.com/zegit-zoo/meerkat/internal/operator/webhook"
)

// fieldOwner is the server-side apply manager for everything the
// operator renders.
const fieldOwner = "meerkat-operator"

// The reconciler reads MeerkatMobs, KnowledgeBases and the auth
// ConfigMaps they name, and renders a ConfigMap, a Deployment and a
// Service per placement unit.
//
// +kubebuilder:rbac:groups=meerkat-mob.dev,resources=meerkatmobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=meerkat-mob.dev,resources=meerkatmobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=meerkat-mob.dev,resources=meerkatmobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;configmaps,verbs=get;list;watch;create;update;patch;delete

// MeerkatMobReconciler renders each MeerkatMob's tree as meerkat
// instances, one per placement unit, and reports the result in status.
type MeerkatMobReconciler struct {
	client.Client
	// Images decides the image the instances run.
	Images *imagepolicy.Policy
}

// refusal is why a mob was not rendered.
type refusal struct {
	reason, message string
}

// plan is what a mob renders to.
type plan struct {
	image string
	kbs   int
	units []plannedUnit
}

type plannedUnit struct {
	unit
	config []byte
}

// Reconcile renders one MeerkatMob.
func (r *MeerkatMobReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var mob v1alpha1.MeerkatMob
	if err := r.Get(ctx, req.NamespacedName, &mob); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !mob.DeletionTimestamp.IsZero() {
		// The owner references hand the rest to the garbage collector.
		return reconcile.Result{}, nil
	}

	status := *mob.Status.DeepCopy()
	status.ObservedGeneration = mob.Generation
	p, ref, err := r.plan(ctx, &mob)
	if err != nil {
		return reconcile.Result{}, err
	}
	if ref != nil {
		// Leave whatever the last good spec rendered running: a typo in
		// a KnowledgeBase must not take the mob's instances down.
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionRendered, Status: metav1.ConditionFalse,
			Reason: ref.reason, Message: ref.message, ObservedGeneration: mob.Generation,
		})
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionAvailable, Status: metav1.ConditionUnknown,
			Reason: v1alpha1.ReasonNotRendered, Message: "the current spec was not rendered; see the Rendered condition",
			ObservedGeneration: mob.Generation,
		})
		return reconcile.Result{}, r.writeStatus(ctx, &mob, status)
	}

	want := map[string]bool{}
	for i := range p.units {
		u := &p.units[i]
		want[u.Name] = true
		for _, obj := range []runtime.ApplyConfiguration{
			configMapFor(&mob, &u.unit, u.config),
			deploymentFor(&mob, &u.unit, p.image, u.config),
			serviceFor(&mob, &u.unit),
		} {
			if err := r.Apply(ctx, obj, client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
				return reconcile.Result{}, fmt.Errorf("applying %s for MeerkatMob %s: %w", u.Name, req.NamespacedName, err)
			}
		}
	}
	if err := r.prune(ctx, &mob, want); err != nil {
		return reconcile.Result{}, err
	}

	status.Image = p.image
	status.Instances = count32(len(p.units))
	status.KnowledgeBases = count32(p.kbs)
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionRendered, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonRendered,
		Message:            fmt.Sprintf("%d KnowledgeBases in %d instances", p.kbs, len(p.units)),
		ObservedGeneration: mob.Generation,
	})
	avail, err := r.availability(ctx, &mob, p.units)
	if err != nil {
		return reconcile.Result{}, err
	}
	avail.ObservedGeneration = mob.Generation
	meta.SetStatusCondition(&status.Conditions, avail)
	return reconcile.Result{}, r.writeStatus(ctx, &mob, status)
}

// plan resolves the image, checks the tree and renders every unit's
// content-source.yaml. A refusal is a spec problem to report; an error
// is a failed read to retry.
func (r *MeerkatMobReconciler) plan(ctx context.Context, mob *v1alpha1.MeerkatMob) (*plan, *refusal, error) {
	image, err := r.Images.Resolve(mob.Spec.Image)
	if err != nil {
		return nil, &refusal{v1alpha1.ReasonImageRefused, err.Error()}, nil
	}
	var list v1alpha1.KnowledgeBaseList
	if err := r.List(ctx, &list, client.InNamespace(mob.Namespace)); err != nil {
		return nil, nil, fmt.Errorf("listing KnowledgeBases in %q: %w", mob.Namespace, err)
	}
	kbs, ok := subtree(mob.Spec.Root, list.Items)
	if !ok {
		return nil, &refusal{v1alpha1.ReasonRootMissing, fmt.Sprintf("KnowledgeBase %q does not exist", mob.Spec.Root)}, nil
	}
	// Admission is time-of-check (see the KnowledgeBase reconciler), so
	// the tree rules run again here, on the namespace as it is now.
	for i := range kbs {
		if _, err := webhook.CheckTree(&kbs[i], list.Items); err != nil {
			return nil, &refusal{v1alpha1.ReasonTreeRefused, err.Error()}, nil
		}
	}
	if mob.Spec.Auth == nil {
		return nil, &refusal{v1alpha1.ReasonConfigRefused, "spec.auth is not set: the operator never starts a hosted server without authentication"}, nil
	}
	var authYAML string
	if name := mob.Spec.Auth.ConfigMapRef; name != "" {
		var cm corev1.ConfigMap
		err := r.Get(ctx, types.NamespacedName{Namespace: mob.Namespace, Name: name}, &cm)
		switch {
		case client.IgnoreNotFound(err) != nil:
			return nil, nil, fmt.Errorf("reading ConfigMap %s/%s: %w", mob.Namespace, name, err)
		case err != nil:
			return nil, &refusal{v1alpha1.ReasonConfigRefused, fmt.Sprintf("spec.auth.configMapRef: ConfigMap %q does not exist", name)}, nil
		}
		v, ok := cm.Data[v1alpha1.AuthConfigMapKey]
		if !ok {
			return nil, &refusal{v1alpha1.ReasonConfigRefused, fmt.Sprintf("spec.auth.configMapRef: ConfigMap %q has no %s key", name, v1alpha1.AuthConfigMapKey)}, nil
		}
		authYAML = v
	}
	auth, err := renderAuth(mob.Spec.Auth, authYAML)
	if err != nil {
		return nil, &refusal{v1alpha1.ReasonConfigRefused, err.Error()}, nil
	}
	obs := renderTelemetry(mob.Spec.Telemetry)

	p := &plan{image: image, kbs: len(kbs)}
	for _, u := range planUnits(mob, kbs) {
		body, err := renderConfig(&u, auth, obs)
		if err != nil {
			return nil, &refusal{v1alpha1.ReasonConfigRefused, err.Error()}, nil
		}
		p.units = append(p.units, plannedUnit{unit: u, config: body})
	}
	return p, nil, nil
}

// prune deletes what mob rendered for a unit it no longer has.
func (r *MeerkatMobReconciler) prune(ctx context.Context, mob *v1alpha1.MeerkatMob, want map[string]bool) error {
	sel := client.MatchingLabels(mobLabels(mob))
	var deps appsv1.DeploymentList
	var svcs corev1.ServiceList
	var cms corev1.ConfigMapList
	for _, l := range []client.ObjectList{&deps, &svcs, &cms} {
		if err := r.List(ctx, l, client.InNamespace(mob.Namespace), sel); err != nil {
			return fmt.Errorf("listing what MeerkatMob %s/%s rendered: %w", mob.Namespace, mob.Name, err)
		}
	}
	var stale []client.Object
	for i := range deps.Items {
		stale = append(stale, &deps.Items[i])
	}
	for i := range svcs.Items {
		stale = append(stale, &svcs.Items[i])
	}
	for i := range cms.Items {
		stale = append(stale, &cms.Items[i])
	}
	for _, obj := range stale {
		if want[obj.GetName()] || !metav1.IsControlledBy(obj, mob) {
			continue
		}
		if err := r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return nil
}

// availability is the Available condition: True once every unit's
// Deployment has rolled out its current generation with an available
// replica.
func (r *MeerkatMobReconciler) availability(ctx context.Context, mob *v1alpha1.MeerkatMob, units []plannedUnit) (metav1.Condition, error) {
	var waiting []string
	for i := range units {
		var d appsv1.Deployment
		err := r.Get(ctx, types.NamespacedName{Namespace: mob.Namespace, Name: units[i].Name}, &d)
		if client.IgnoreNotFound(err) != nil {
			return metav1.Condition{}, fmt.Errorf("reading Deployment %s/%s: %w", mob.Namespace, units[i].Name, err)
		}
		if err != nil || d.Status.ObservedGeneration < d.Generation || d.Status.UpdatedReplicas < 1 || d.Status.AvailableReplicas < 1 {
			waiting = append(waiting, units[i].Name)
		}
	}
	if len(waiting) > 0 {
		return metav1.Condition{
			Type: v1alpha1.ConditionAvailable, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonProgressing,
			Message: fmt.Sprintf("waiting for %d of %d instances: %v", len(waiting), len(units), waiting),
		}, nil
	}
	return metav1.Condition{
		Type: v1alpha1.ConditionAvailable, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonAvailable,
		Message: fmt.Sprintf("all %d instances are available", len(units)),
	}, nil
}

// count32 is n as a status count.
func count32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n) // #nosec G115 -- bounded above
}

// writeStatus updates mob's status when it changed.
func (r *MeerkatMobReconciler) writeStatus(ctx context.Context, mob *v1alpha1.MeerkatMob, status v1alpha1.MeerkatMobStatus) error {
	if equality.Semantic.DeepEqual(status, mob.Status) {
		return nil
	}
	mob.Status = status
	if err := r.Status().Update(ctx, mob); err != nil {
		return fmt.Errorf("updating MeerkatMob %s/%s status: %w", mob.Namespace, mob.Name, err)
	}
	return nil
}

// SetupWithManager registers the reconciler. A mob depends on every
// KnowledgeBase in its namespace (any of them can join or leave its
// tree) and on its auth ConfigMap, so changes to those re-queue it, as
// do changes to what it rendered: a Deployment's status decides
// Available, and an edit or delete by hand is put back.
func (r *MeerkatMobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.MeerkatMob{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&v1alpha1.KnowledgeBase{},
			handler.EnqueueRequestsFromMapFunc(r.mobsInNamespace),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.mobsUsingAuthConfigMap)).
		Named("meerkatmob").
		Complete(r)
}

// mobsInNamespace lists every MeerkatMob in obj's namespace.
func (r *MeerkatMobReconciler) mobsInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.mobsWhere(ctx, obj.GetNamespace(), func(*v1alpha1.MeerkatMob) bool { return true })
}

// mobsUsingAuthConfigMap lists the MeerkatMobs whose spec.auth names
// obj.
func (r *MeerkatMobReconciler) mobsUsingAuthConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.mobsWhere(ctx, obj.GetNamespace(), func(m *v1alpha1.MeerkatMob) bool {
		return m.Spec.Auth != nil && m.Spec.Auth.ConfigMapRef == obj.GetName()
	})
}

func (r *MeerkatMobReconciler) mobsWhere(ctx context.Context, ns string, keep func(*v1alpha1.MeerkatMob) bool) []reconcile.Request {
	var list v1alpha1.MeerkatMobList
	if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing MeerkatMobs to re-queue", "namespace", ns)
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		if keep(&list.Items[i]) {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: list.Items[i].Name}})
		}
	}
	return out
}
