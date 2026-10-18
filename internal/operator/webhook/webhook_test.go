package webhook

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
)

const ns = "kb"

func kb(name, parent string) *v1alpha1.KnowledgeBase {
	return &v1alpha1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.KnowledgeBaseSpec{
			Parent: parent,
			Source: v1alpha1.KnowledgeBaseSource{S3: &v1alpha1.S3Source{Bucket: "kb", Prefix: name + "/"}},
		},
	}
}

func withLimit(k *v1alpha1.KnowledgeBase, l int32) *v1alpha1.KnowledgeBase {
	k.Spec.DepthLimit = &l
	return k
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func reader(t *testing.T, objs ...*v1alpha1.KnowledgeBase) client.Reader {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(newScheme(t))
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return b.Build()
}

// chain returns root, c1, ..., cn, each the parent of the next.
func chain(n int) []*v1alpha1.KnowledgeBase {
	out := []*v1alpha1.KnowledgeBase{kb("root", "")}
	for i := 1; i <= n; i++ {
		out = append(out, kb("c"+string(rune('0'+i)), out[i-1].Name))
	}
	return out
}

func wantErr(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("err = %v, want one containing %q", err, sub)
	}
}

func TestMaxDepthIsTheManifestLoadersCap(t *testing.T) {
	// The webhook and the server's tree loader must refuse the same
	// trees; issue #10 fixes the cap at 5.
	webhookCap, loaderCap := MaxDepth, contentsource.MaxTreeDepth
	if webhookCap != loaderCap || webhookCap != 5 {
		t.Fatalf("MaxDepth = %d, contentsource.MaxTreeDepth = %d, want both 5", webhookCap, loaderCap)
	}
}

func TestKnowledgeBase_DepthFiveIsAdmittedSixIsNot(t *testing.T) {
	ctx := context.Background()
	c := chain(5)
	v := &KnowledgeBaseValidator{Reader: reader(t, c[:5]...)}
	if w, err := v.ValidateCreate(ctx, c[5]); err != nil || len(w) != 0 {
		t.Fatalf("depth 5: warnings %v, err %v; want admitted cleanly", w, err)
	}

	v = &KnowledgeBaseValidator{Reader: reader(t, c...)}
	_, err := v.ValidateCreate(ctx, kb("c6", "c5"))
	wantErr(t, err, "depth 6")
	wantErr(t, err, "root/c1/c2/c3/c4/c5/c6")
}

func TestKnowledgeBase_SelfParentIsRefused(t *testing.T) {
	v := &KnowledgeBaseValidator{Reader: reader(t)}
	_, err := v.ValidateCreate(context.Background(), kb("a", "a"))
	wantErr(t, err, "names itself")
}

func TestKnowledgeBase_CycleIsRefused(t *testing.T) {
	ctx := context.Background()
	// a <- b <- c, then a is re-parented under c.
	a, b, c := kb("a", ""), kb("b", "a"), kb("c", "b")
	v := &KnowledgeBaseValidator{Reader: reader(t, a, b, c)}
	_, err := v.ValidateUpdate(ctx, a, kb("a", "c"))
	wantErr(t, err, "loops")
	wantErr(t, err, "a -> c -> b -> a")
}

func TestKnowledgeBase_CycleClosedByCreatingTheMissingParent(t *testing.T) {
	// b was admitted naming a missing parent x; creating x under b
	// closes the loop and is refused.
	v := &KnowledgeBaseValidator{Reader: reader(t, kb("b", "x"))}
	_, err := v.ValidateCreate(context.Background(), kb("x", "b"))
	wantErr(t, err, "loops")
}

func TestKnowledgeBase_ReparentingChecksTheSubtreeBelow(t *testing.T) {
	ctx := context.Background()
	// root - c1 - c2 - c3 (depth 3), and a separate x - y - z.
	c := chain(3)
	x, y, z := kb("x", ""), kb("y", "x"), kb("z", "y")
	objs := append(c, x, y, z)
	v := &KnowledgeBaseValidator{Reader: reader(t, objs...)}

	// x under c2 puts z at depth 5: admitted.
	if _, err := v.ValidateUpdate(ctx, x, kb("x", "c2")); err != nil {
		t.Fatalf("x under c2: %v", err)
	}
	// x under c3 puts z at depth 6: refused, and the error names z,
	// which the edit did not touch.
	_, err := v.ValidateUpdate(ctx, x, kb("x", "c3"))
	wantErr(t, err, `"z" would be at depth 6`)
}

func TestKnowledgeBase_DepthLimitLowersTheCapBelowIt(t *testing.T) {
	ctx := context.Background()
	// c1 declares depthLimit 1: its children may sit one level below it
	// (depth 2), not two.
	root, c1 := kb("root", ""), withLimit(kb("c1", "root"), 1)
	c2 := kb("c2", "c1")
	v := &KnowledgeBaseValidator{Reader: reader(t, root, c1, c2)}
	if _, err := v.ValidateCreate(ctx, kb("other", "c1")); err != nil {
		t.Fatalf("a child one level below the limit's owner: %v", err)
	}
	_, err := v.ValidateCreate(ctx, kb("c3", "c2"))
	wantErr(t, err, "over the depthLimit 2")

	// Setting the limit on an existing KnowledgeBase checks what is
	// already below it.
	_, err = v.ValidateUpdate(ctx, root, withLimit(kb("root", ""), 1))
	wantErr(t, err, `"c2" would be at depth 2`)
}

func TestKnowledgeBase_DepthLimitNeverRaisesTheCap(t *testing.T) {
	c := chain(4)
	withLimit(c[4], 5) // c4 at depth 4 asks for five more levels
	v := &KnowledgeBaseValidator{Reader: reader(t, c...)}
	if _, err := v.ValidateCreate(context.Background(), kb("c5", "c4")); err != nil {
		t.Fatalf("depth 5: %v", err)
	}
	v = &KnowledgeBaseValidator{Reader: reader(t, append(c, kb("c5", "c4"))...)}
	_, err := v.ValidateCreate(context.Background(), kb("c6", "c5"))
	wantErr(t, err, "hard cap")
}

func TestKnowledgeBase_MissingParentIsAdmittedWithAWarning(t *testing.T) {
	// kubectl apply -f dir/ may create a child before its parent.
	v := &KnowledgeBaseValidator{Reader: reader(t)}
	w, err := v.ValidateCreate(context.Background(), kb("child", "later"))
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 1 || !strings.Contains(w[0], `"later" does not exist yet`) {
		t.Fatalf("warnings = %v", w)
	}
}

func TestKnowledgeBase_MissingParentStillCountsAsOneLevel(t *testing.T) {
	// A chain of five under a missing parent is at depth 6 at least.
	objs := []*v1alpha1.KnowledgeBase{kb("c1", "missing")}
	for i := 2; i <= 5; i++ {
		objs = append(objs, kb("c"+string(rune('0'+i)), objs[i-2].Name))
	}
	v := &KnowledgeBaseValidator{Reader: reader(t, objs...)}
	_, err := v.ValidateCreate(context.Background(), kb("c6", "c5"))
	wantErr(t, err, "depth 6")
}

func TestKnowledgeBase_UnrelatedPreexistingViolationDoesNotBlock(t *testing.T) {
	// A tree admitted while the webhook was down is six deep; adding a
	// sibling at depth 1 is not this edit's problem.
	c := chain(6)
	v := &KnowledgeBaseValidator{Reader: reader(t, c...)}
	if _, err := v.ValidateCreate(context.Background(), kb("side", "root")); err != nil {
		t.Fatalf("unrelated sibling: %v", err)
	}
}

func TestKnowledgeBase_SourceMustBeExactlyOne(t *testing.T) {
	v := &KnowledgeBaseValidator{Reader: reader(t)}
	k := kb("a", "")
	k.Spec.Source.GCS = &v1alpha1.GCSSource{Bucket: "g"}
	_, err := v.ValidateCreate(context.Background(), k)
	wantErr(t, err, "exactly one")

	k.Spec.Source = v1alpha1.KnowledgeBaseSource{}
	_, err = v.ValidateCreate(context.Background(), k)
	wantErr(t, err, "got 0")
}

func TestKnowledgeBase_PlaintextS3NeedsInsecure(t *testing.T) {
	v := &KnowledgeBaseValidator{Reader: reader(t)}
	k := kb("a", "")
	k.Spec.Source.S3.Endpoint = "http://garage:3900"
	_, err := v.ValidateCreate(context.Background(), k)
	wantErr(t, err, "insecure: true")

	k.Spec.Source.S3.Insecure = true
	if _, err := v.ValidateCreate(context.Background(), k); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeBase_ListFailureFailsClosed(t *testing.T) {
	r := fake.NewClientBuilder().WithScheme(newScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("apiserver unavailable")
		},
	}).Build()
	v := &KnowledgeBaseValidator{Reader: r}
	_, err := v.ValidateCreate(context.Background(), kb("a", ""))
	wantErr(t, err, "apiserver unavailable")

	// A delete is never blocked on it.
	if _, err := v.ValidateDelete(context.Background(), kb("a", "")); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestKnowledgeBase_DeleteWarnsAboutOrphans(t *testing.T) {
	root, a, b := kb("root", ""), kb("a", "root"), kb("b", "root")
	v := &KnowledgeBaseValidator{Reader: reader(t, root, a, b)}
	w, err := v.ValidateDelete(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 1 || !strings.Contains(w[0], "a, b") {
		t.Fatalf("warnings = %v", w)
	}
	if w, _ := v.ValidateDelete(context.Background(), a); len(w) != 0 {
		t.Fatalf("a leaf delete warned: %v", w)
	}
}

func mob(root string) *v1alpha1.MeerkatMob {
	return &v1alpha1.MeerkatMob{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: ns},
		Spec:       v1alpha1.MeerkatMobSpec{Root: root},
	}
}

func TestMeerkatMob(t *testing.T) {
	ctx := context.Background()
	v := &MeerkatMobValidator{Reader: reader(t, kb("root", ""), kb("child", "root"))}

	if w, err := v.ValidateCreate(ctx, mob("root")); err != nil || len(w) != 0 {
		t.Fatalf("plain mob: %v %v", w, err)
	}
	if w, err := v.ValidateCreate(ctx, mob("nope")); err != nil || len(w) != 1 || !strings.Contains(w[0], "does not exist yet") {
		t.Fatalf("missing root: %v %v", w, err)
	}
	if w, err := v.ValidateCreate(ctx, mob("child")); err != nil || len(w) != 1 || !strings.Contains(w[0], "subtree") {
		t.Fatalf("non-root: %v %v", w, err)
	}

	m := mob("root")
	m.Spec.ScaleToZero = &v1alpha1.ScaleToZero{Enabled: true, Idle: &metav1.Duration{Duration: 10 * time.Second}}
	_, err := v.ValidateCreate(ctx, m)
	wantErr(t, err, "at least 1m0s")

	m = mob("root")
	m.Spec.Telemetry = &v1alpha1.Telemetry{Endpoint: "http://collector:4318"}
	_, err = v.ValidateUpdate(ctx, mob("root"), m)
	wantErr(t, err, "insecure: true")

	m = mob("root")
	m.Spec.Librarian = &v1alpha1.Librarian{Schedule: "@daily please"}
	_, err = v.ValidateCreate(ctx, m)
	wantErr(t, err, "five-field")
	m.Spec.Librarian.Schedule = "0 3 * * *"
	if _, err := v.ValidateCreate(ctx, m); err != nil {
		t.Fatal(err)
	}
}
