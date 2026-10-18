package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
)

const ns = "kb"

func kb(name, parent string) *v1alpha1.KnowledgeBase {
	return &v1alpha1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Generation: 1},
		Spec: v1alpha1.KnowledgeBaseSpec{
			Source: v1alpha1.KnowledgeBaseSource{S3: &v1alpha1.S3Source{Bucket: "kb"}},
			Parent: parent,
		},
	}
}

func newReconciler(t *testing.T, objs ...client.Object) *KnowledgeBaseReconciler {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.KnowledgeBase{}).Build()
	return &KnowledgeBaseReconciler{Client: c}
}

// reconcileKB reconciles name and returns it as stored afterwards.
func reconcileKB(t *testing.T, r *KnowledgeBaseReconciler, name string) *v1alpha1.KnowledgeBase {
	t.Helper()
	key := types.NamespacedName{Namespace: ns, Name: name}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	var got v1alpha1.KnowledgeBase
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func wantCondition(t *testing.T, got *v1alpha1.KnowledgeBase, status metav1.ConditionStatus, reason, msg string) {
	t.Helper()
	c := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionTreeValid)
	if c == nil {
		t.Fatalf("%s: no %s condition in %+v", got.Name, v1alpha1.ConditionTreeValid, got.Status)
	}
	if c.Status != status || c.Reason != reason || !strings.Contains(c.Message, msg) {
		t.Errorf("%s: condition = %s/%s %q, want %s/%s containing %q", got.Name, c.Status, c.Reason, c.Message, status, reason, msg)
	}
	if c.ObservedGeneration != got.Generation {
		t.Errorf("%s: condition observedGeneration = %d, want %d", got.Name, c.ObservedGeneration, got.Generation)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("%s: status.observedGeneration = %d, want %d", got.Name, got.Status.ObservedGeneration, got.Generation)
	}
}

func TestKnowledgeBase_ValidTreeIsTreeValid(t *testing.T) {
	r := newReconciler(t, kb("root", ""), kb("platform", "root"), kb("flux", "platform"))
	for name, depth := range map[string]int32{"root": 0, "platform": 1, "flux": 2} {
		got := reconcileKB(t, r, name)
		wantCondition(t, got, metav1.ConditionTrue, v1alpha1.ReasonTreeValid, "")
		if got.Status.Depth != depth {
			t.Errorf("%s: depth = %d, want %d", name, got.Status.Depth, depth)
		}
	}
}

func TestKnowledgeBase_LoopAdmittedConcurrentlyIsRefused(t *testing.T) {
	// Two creates that each passed the webhook alone: a names b, and b
	// names a. Neither saw the other at admission.
	r := newReconciler(t, kb("a", "b"), kb("b", "a"))
	for _, name := range []string{"a", "b"} {
		wantCondition(t, reconcileKB(t, r, name), metav1.ConditionFalse, v1alpha1.ReasonTreeRefused, "loops")
	}
}

func TestKnowledgeBase_TooDeepIsRefused(t *testing.T) {
	objs := []client.Object{kb("c0", "")}
	for i := 1; i <= 6; i++ {
		objs = append(objs, kb(fmt.Sprintf("c%d", i), fmt.Sprintf("c%d", i-1)))
	}
	r := newReconciler(t, objs...)
	got := reconcileKB(t, r, "c6")
	wantCondition(t, got, metav1.ConditionFalse, v1alpha1.ReasonTreeRefused, "hard cap")
	if got.Status.Depth != 6 {
		t.Errorf("depth = %d, want 6", got.Status.Depth)
	}
	// c5 sits at the cap; it is c6 that breaks it, so c5 itself is
	// refused too: its subtree is over the cap.
	wantCondition(t, reconcileKB(t, r, "c5"), metav1.ConditionFalse, v1alpha1.ReasonTreeRefused, "c6")
	wantCondition(t, reconcileKB(t, r, "c0"), metav1.ConditionFalse, v1alpha1.ReasonTreeRefused, "c6")
}

func TestKnowledgeBase_MissingParentIsUnknown(t *testing.T) {
	r := newReconciler(t, kb("a", "x"))
	got := reconcileKB(t, r, "a")
	wantCondition(t, got, metav1.ConditionUnknown, v1alpha1.ReasonParentMissing, `"x" does not exist yet`)
	if got.Status.Depth != 1 {
		t.Errorf("depth = %d, want 1 (under a missing parent)", got.Status.Depth)
	}
}

func TestKnowledgeBase_FixingTheTreeClearsTheCondition(t *testing.T) {
	r := newReconciler(t, kb("a", "b"), kb("b", "a"))
	wantCondition(t, reconcileKB(t, r, "a"), metav1.ConditionFalse, v1alpha1.ReasonTreeRefused, "loops")

	var b v1alpha1.KnowledgeBase
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "b"}, &b); err != nil {
		t.Fatal(err)
	}
	b.Spec.Parent = ""
	if err := r.Update(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	got := reconcileKB(t, r, "a")
	wantCondition(t, got, metav1.ConditionTrue, v1alpha1.ReasonTreeValid, "")
	if got.Status.Depth != 1 {
		t.Errorf("depth = %d, want 1", got.Status.Depth)
	}
}

func TestKnowledgeBase_UnchangedStatusIsNotWritten(t *testing.T) {
	r := newReconciler(t, kb("root", ""))
	first := reconcileKB(t, r, "root")
	second := reconcileKB(t, r, "root")
	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("a second reconcile with nothing changed wrote status (resourceVersion %s -> %s)", first.ResourceVersion, second.ResourceVersion)
	}
}

func TestKnowledgeBase_NewGenerationIsObserved(t *testing.T) {
	r := newReconciler(t, kb("root", ""))
	reconcileKB(t, r, "root")
	var got v1alpha1.KnowledgeBase
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "root"}, &got); err != nil {
		t.Fatal(err)
	}
	got.Generation = 2
	if err := r.Update(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	wantCondition(t, reconcileKB(t, r, "root"), metav1.ConditionTrue, v1alpha1.ReasonTreeValid, "")
}

func TestKnowledgeBase_GoneOrDeletingIsIgnored(t *testing.T) {
	r := newReconciler(t)
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "gone"}}); err != nil {
		t.Errorf("reconcile of a missing KnowledgeBase: %v", err)
	}

	deleting := kb("a", "")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"example.net/hold"}
	r = newReconciler(t, deleting)
	if got := reconcileKB(t, r, "a"); len(got.Status.Conditions) != 0 {
		t.Errorf("a deleting KnowledgeBase got status %+v", got.Status)
	}
}

func TestKnowledgeBase_NamespacePeers(t *testing.T) {
	other := kb("elsewhere", "")
	other.Namespace = "other"
	r := newReconciler(t, kb("root", ""), kb("a", "root"), kb("b", "root"), other)
	got := r.namespacePeers(context.Background(), kb("a", "root"))
	var names []string
	for _, req := range got {
		if req.Namespace != ns {
			t.Errorf("peer %s is outside namespace %s", req, ns)
		}
		names = append(names, req.Name)
	}
	if strings.Join(names, ",") != "b,root" {
		t.Errorf("peers of a = %v, want [b root]", names)
	}
}
