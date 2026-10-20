package controller

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
)

// mobNS keeps the MeerkatMob subtests apart from the KnowledgeBase
// ones, which build broken trees in "default".
const mobNS = "mobs"

type mobHarness struct {
	ctx context.Context
	c   client.Client
}

func newMobHarness(ctx context.Context, t *testing.T, c client.Client) *mobHarness {
	t.Helper()
	h := &mobHarness{ctx: ctx, c: c}
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: mobNS}}); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "mob-auth", Namespace: mobNS},
		Data:       map[string]string{v1alpha1.AuthConfigMapKey: "resource: https://meerkat.example/mcp\nrules:\n  - name: all\n    collections: ['*']\n"},
	}
	if err := c.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	for _, k := range []struct {
		name, parent string
		tier         int32
	}{{"root", "", 0}, {"platform", "root", 1}, {"vendors", "root", 1}} {
		kb := &v1alpha1.KnowledgeBase{
			ObjectMeta: metav1.ObjectMeta{Name: k.name, Namespace: mobNS},
			Spec: v1alpha1.KnowledgeBaseSpec{
				Parent: k.parent,
				Tier:   k.tier,
				Source: v1alpha1.KnowledgeBaseSource{S3: &v1alpha1.S3Source{Bucket: "kb", Prefix: k.name + "/", Endpoint: "https://s3.example.net", PathStyle: true}},
			},
		}
		if err := c.Create(ctx, kb); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *mobHarness) newMob(name, placement string) *v1alpha1.MeerkatMob {
	return &v1alpha1.MeerkatMob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mobNS},
		Spec: v1alpha1.MeerkatMobSpec{
			Root:      "root",
			Placement: placement,
			Auth:      &v1alpha1.Auth{Issuer: "https://issuer.example", Audience: "meerkat", ConfigMapRef: "mob-auth"},
		},
	}
}

// eventually polls cond for up to 30s.
func eventually(t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ok, detail := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 30s; %s", what, detail)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitMob polls a mob until condType has status and reason for its
// current generation.
func (h *mobHarness) waitMob(t *testing.T, name, condType string, status metav1.ConditionStatus, reason string) *v1alpha1.MeerkatMob {
	t.Helper()
	var got v1alpha1.MeerkatMob
	eventually(t, name+" "+condType+"="+string(status)+"/"+reason, func() (bool, string) {
		if err := h.c.Get(h.ctx, types.NamespacedName{Namespace: mobNS, Name: name}, &got); err != nil {
			return false, err.Error()
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, condType)
		return cond != nil && cond.Status == status && cond.Reason == reason &&
			cond.ObservedGeneration == got.Generation && got.Status.ObservedGeneration == got.Generation, "status " + spew(got.Status)
	})
	return &got
}

func spew(s v1alpha1.MeerkatMobStatus) string {
	var b strings.Builder
	for _, c := range s.Conditions {
		b.WriteString(c.Type + "=" + string(c.Status) + "/" + c.Reason + ": " + c.Message + "; ")
	}
	return b.String()
}

// rendered lists the names of the Deployments, Services and ConfigMaps
// mob controls, each sorted.
func (h *mobHarness) rendered(t *testing.T, mob *v1alpha1.MeerkatMob) (deps, svcs, cms []string) {
	t.Helper()
	var dl appsv1.DeploymentList
	var sl corev1.ServiceList
	var cl corev1.ConfigMapList
	for _, l := range []client.ObjectList{&dl, &sl, &cl} {
		if err := h.c.List(h.ctx, l, client.InNamespace(mobNS)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range dl.Items {
		if metav1.IsControlledBy(&dl.Items[i], mob) && dl.Items[i].DeletionTimestamp.IsZero() {
			deps = append(deps, dl.Items[i].Name)
		}
	}
	for i := range sl.Items {
		if metav1.IsControlledBy(&sl.Items[i], mob) && sl.Items[i].DeletionTimestamp.IsZero() {
			svcs = append(svcs, sl.Items[i].Name)
		}
	}
	for i := range cl.Items {
		if metav1.IsControlledBy(&cl.Items[i], mob) && cl.Items[i].DeletionTimestamp.IsZero() {
			cms = append(cms, cl.Items[i].Name)
		}
	}
	sort.Strings(deps)
	sort.Strings(svcs)
	sort.Strings(cms)
	return deps, svcs, cms
}

func (h *mobHarness) waitRendered(t *testing.T, mob *v1alpha1.MeerkatMob, want string) {
	t.Helper()
	eventually(t, mob.Name+" renders "+want, func() (bool, string) {
		deps, svcs, cms := h.rendered(t, mob)
		got := strings.Join(deps, ",")
		return got == want && strings.Join(svcs, ",") == want && strings.Join(cms, ",") == want,
			"deployments " + got + ", services " + strings.Join(svcs, ",") + ", configmaps " + strings.Join(cms, ",")
	})
}

func (h *mobHarness) perKB(t *testing.T) {
	mob := h.newMob("tree", v1alpha1.PlacementPerKB)
	if err := h.c.Create(h.ctx, mob); err != nil {
		t.Fatal(err)
	}
	got := h.waitMob(t, "tree", v1alpha1.ConditionRendered, metav1.ConditionTrue, v1alpha1.ReasonRendered)
	if got.Status.Instances != 3 || got.Status.KnowledgeBases != 3 || got.Status.Image != "ghcr.io/zegit-zoo/meerkat:0.21.0" {
		t.Errorf("status = %+v", got.Status)
	}
	h.waitMob(t, "tree", v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonProgressing)
	h.waitRendered(t, got, "tree-kb-platform,tree-kb-root,tree-kb-vendors")

	var cm corev1.ConfigMap
	if err := h.c.Get(h.ctx, types.NamespacedName{Namespace: mobNS, Name: "tree-kb-platform"}, &cm); err != nil {
		t.Fatal(err)
	}
	body := cm.Data[contentSourceKey]
	if !strings.Contains(body, "name: platform") || strings.Contains(body, "name: root") || !strings.Contains(body, "https://issuer.example") {
		t.Errorf("tree-kb-platform's content-source.yaml:\n%s", body)
	}
	var d appsv1.Deployment
	if err := h.c.Get(h.ctx, types.NamespacedName{Namespace: mobNS, Name: "tree-kb-platform"}, &d); err != nil {
		t.Fatal(err)
	}
	if d.Spec.Template.Spec.Containers[0].Image != "ghcr.io/zegit-zoo/meerkat:0.21.0" {
		t.Errorf("image = %s", d.Spec.Template.Spec.Containers[0].Image)
	}

	// A hand edit is put back: the reconciler owns the Deployment.
	d.Spec.Template.Spec.Containers[0].Image = "ghcr.io/attacker/meerkat:1"
	if err := h.c.Update(h.ctx, &d); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the hand-edited image is put back", func() (bool, string) {
		if err := h.c.Get(h.ctx, types.NamespacedName{Namespace: mobNS, Name: "tree-kb-platform"}, &d); err != nil {
			return false, err.Error()
		}
		img := d.Spec.Template.Spec.Containers[0].Image
		return img == "ghcr.io/zegit-zoo/meerkat:0.21.0", img
	})

	// A new KnowledgeBase under the root gets its own instance.
	kb := &v1alpha1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "flux", Namespace: mobNS},
		Spec: v1alpha1.KnowledgeBaseSpec{Parent: "platform", Tier: 2,
			Source: v1alpha1.KnowledgeBaseSource{S3: &v1alpha1.S3Source{Bucket: "kb", Prefix: "flux/", Endpoint: "https://s3.example.net"}}},
	}
	if err := h.c.Create(h.ctx, kb); err != nil {
		t.Fatal(err)
	}
	h.waitRendered(t, got, "tree-kb-flux,tree-kb-platform,tree-kb-root,tree-kb-vendors")
}

func (h *mobHarness) toSingle(t *testing.T) {
	var mob v1alpha1.MeerkatMob
	if err := h.c.Get(h.ctx, types.NamespacedName{Namespace: mobNS, Name: "tree"}, &mob); err != nil {
		t.Fatal(err)
	}
	mob.Spec.Placement = v1alpha1.PlacementSingle
	if err := h.c.Update(h.ctx, &mob); err != nil {
		t.Fatal(err)
	}
	got := h.waitMob(t, "tree", v1alpha1.ConditionRendered, metav1.ConditionTrue, v1alpha1.ReasonRendered)
	if got.Status.Instances != 1 || got.Status.KnowledgeBases != 4 {
		t.Errorf("status = %+v", got.Status)
	}
	h.waitRendered(t, got, "tree-all")
	var cm corev1.ConfigMap
	if err := h.c.Get(h.ctx, types.NamespacedName{Namespace: mobNS, Name: "tree-all"}, &cm); err != nil {
		t.Fatal(err)
	}
	body := cm.Data[contentSourceKey]
	// Tree order: root, then platform and the flux below it, then vendors.
	order := []string{"name: root", "name: platform", "name: flux", "name: vendors"}
	last := -1
	for _, o := range order {
		i := strings.Index(body, o)
		if i <= last {
			t.Fatalf("collections are not in tree order:\n%s", body)
		}
		last = i
	}
}

func (h *mobHarness) available(t *testing.T) {
	// envtest runs no Deployment controller: write the status a rolled
	// out Deployment would have.
	var d appsv1.Deployment
	if err := h.c.Get(h.ctx, types.NamespacedName{Namespace: mobNS, Name: "tree-all"}, &d); err != nil {
		t.Fatal(err)
	}
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	if err := h.c.Status().Update(h.ctx, &d); err != nil {
		t.Fatal(err)
	}
	h.waitMob(t, "tree", v1alpha1.ConditionAvailable, metav1.ConditionTrue, v1alpha1.ReasonAvailable)
}

func (h *mobHarness) rootMissing(t *testing.T) {
	mob := h.newMob("other", v1alpha1.PlacementSingle)
	if err := h.c.Create(h.ctx, mob); err != nil {
		t.Fatal(err)
	}
	h.waitRendered(t, h.waitMob(t, "other", v1alpha1.ConditionRendered, metav1.ConditionTrue, v1alpha1.ReasonRendered), "other-all")

	if err := h.c.Get(h.ctx, types.NamespacedName{Namespace: mobNS, Name: "other"}, mob); err != nil {
		t.Fatal(err)
	}
	mob.Spec.Root = "gone"
	if err := h.c.Update(h.ctx, mob); err != nil {
		t.Fatal(err)
	}
	got := h.waitMob(t, "other", v1alpha1.ConditionRendered, metav1.ConditionFalse, v1alpha1.ReasonRootMissing)
	h.waitMob(t, "other", v1alpha1.ConditionAvailable, metav1.ConditionUnknown, v1alpha1.ReasonNotRendered)
	// What the last good spec rendered keeps running.
	if deps, _, _ := h.rendered(t, got); strings.Join(deps, ",") != "other-all" {
		t.Errorf("deployments = %v, want the last good one kept", deps)
	}
}

func (h *mobHarness) imageAdmission(t *testing.T) {
	mob := h.newMob("evil", v1alpha1.PlacementSingle)
	mob.Spec.Image = "ghcr.io/attacker/meerkat:1"
	err := h.c.Create(h.ctx, mob)
	if err == nil || !strings.Contains(err.Error(), "is not allowed") {
		t.Fatalf("create with an image off the allow-list: err = %v, want the webhook's refusal", err)
	}
}

func (h *mobHarness) imageReconciler(t *testing.T) {
	mob := h.newMob("evil", v1alpha1.PlacementSingle)
	mob.Spec.Image = "ghcr.io/attacker/meerkat:1"
	if err := h.c.Create(h.ctx, mob); err != nil {
		t.Fatalf("create with the webhook removed: %v", err)
	}
	got := h.waitMob(t, "evil", v1alpha1.ConditionRendered, metav1.ConditionFalse, v1alpha1.ReasonImageRefused)
	if deps, _, _ := h.rendered(t, got); len(deps) != 0 {
		t.Errorf("a refused image rendered %v", deps)
	}
}
