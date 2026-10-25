package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
	"github.com/zegit-zoo/meerkat/internal/operator/imagepolicy"
	opwebhook "github.com/zegit-zoo/meerkat/internal/operator/webhook"
)

// TestEnvtest runs the reconciler and the webhook against a real API
// server, with the CRDs and the ValidatingWebhookConfiguration that
// `make crds` generates. It needs the envtest binaries: `make
// test-envtest` fetches them and sets KUBEBUILDER_ASSETS. Without
// that, it is skipped.
func TestEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run make test-envtest")
	}
	deploy := filepath.Join("..", "..", "..", "deploy", "operator")
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(deploy, "crds")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{Paths: []string{filepath.Join(deploy, "webhook")}},
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	wo := &env.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 s,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		WebhookServer:          webhook.NewServer(webhook.Options{Host: wo.LocalServingHost, Port: wo.LocalServingPort, CertDir: wo.LocalServingCertDir}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&KnowledgeBaseReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	images, err := imagepolicy.New("ghcr.io/zegit-zoo/meerkat:0.21.0", []string{imagepolicy.DefaultRepository})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&MeerkatMobReconciler{Client: mgr.GetClient(), Images: images}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	if err := opwebhook.Setup(mgr, images); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})

	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	newKB := func(name, parent string) *v1alpha1.KnowledgeBase {
		k := kb(name, parent)
		k.Namespace = "default"
		k.Generation = 0
		return k
	}
	// waitFor polls name until its TreeValid condition for the current
	// generation has the wanted status and reason.
	waitFor := func(t *testing.T, name string, status metav1.ConditionStatus, reason string) *v1alpha1.KnowledgeBase {
		t.Helper()
		var got v1alpha1.KnowledgeBase
		deadline := time.Now().Add(30 * time.Second)
		for {
			if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &got); err == nil {
				cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionTreeValid)
				if cond != nil && cond.Status == status && cond.Reason == reason &&
					cond.ObservedGeneration == got.Generation && got.Status.ObservedGeneration == got.Generation {
					return &got
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no TreeValid %s/%s within 30s; status %+v", name, status, reason, got.Status)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	mobs := newMobHarness(ctx, t, c)

	t.Run("valid tree", func(t *testing.T) {
		for _, k := range []*v1alpha1.KnowledgeBase{newKB("root", ""), newKB("platform", "root")} {
			if err := c.Create(ctx, k); err != nil {
				t.Fatal(err)
			}
		}
		waitFor(t, "root", metav1.ConditionTrue, v1alpha1.ReasonTreeValid)
		if got := waitFor(t, "platform", metav1.ConditionTrue, v1alpha1.ReasonTreeValid); got.Status.Depth != 1 {
			t.Errorf("platform depth = %d, want 1", got.Status.Depth)
		}
	})

	t.Run("webhook refuses a self-parent", func(t *testing.T) {
		// Proves the generated configuration's path reaches the
		// validator the manager serves.
		err := c.Create(ctx, newKB("self", "self"))
		if err == nil || !strings.Contains(err.Error(), "names itself as its parent") {
			t.Fatalf("create self-parent: err = %v, want the webhook's refusal", err)
		}
	})

	t.Run("missing parent then created", func(t *testing.T) {
		if err := c.Create(ctx, newKB("orphan", "later")); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "orphan", metav1.ConditionUnknown, v1alpha1.ReasonParentMissing)
		// Creating the parent re-queues its namespace peers.
		if err := c.Create(ctx, newKB("later", "")); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "orphan", metav1.ConditionTrue, v1alpha1.ReasonTreeValid)
	})

	t.Run("mob renders one instance per KnowledgeBase", mobs.perKB)
	t.Run("mob placement change prunes the old instances", mobs.toSingle)
	t.Run("mob reports availability from its Deployments", mobs.available)
	t.Run("mob without its root keeps what it rendered", mobs.rootMissing)
	t.Run("webhook refuses a mob image off the allow-list", mobs.imageAdmission)
	t.Run("mob credentials and sizing reach the instances", mobs.credentialsAndSizing)
	t.Run("mob refuses to take over a ServiceAccount", mobs.serviceAccountInTheWay)

	t.Run("loop past admission is reported, and clears when fixed", func(t *testing.T) {
		// Stand in for two concurrent creates that each passed the
		// webhook: take the webhook out, then write the loop.
		var vwc admissionregistrationv1.ValidatingWebhookConfigurationList
		if err := c.List(ctx, &vwc); err != nil {
			t.Fatal(err)
		}
		for i := range vwc.Items {
			if err := c.Delete(ctx, &vwc.Items[i]); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.Create(ctx, newKB("a", "b")); err != nil {
			t.Fatal(err)
		}
		// The webhook configuration's deletion may take a moment to
		// reach the API server's admission chain.
		deadline := time.Now().Add(30 * time.Second)
		for {
			err := c.Create(ctx, newKB("b", "a"))
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("create b with the webhook removed: %v", err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		waitFor(t, "a", metav1.ConditionFalse, v1alpha1.ReasonTreeRefused)
		waitFor(t, "b", metav1.ConditionFalse, v1alpha1.ReasonTreeRefused)

		var b v1alpha1.KnowledgeBase
		if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "b"}, &b); err != nil {
			t.Fatal(err)
		}
		b.Spec.Parent = ""
		if err := c.Update(ctx, &b); err != nil {
			t.Fatal(err)
		}
		// a's own spec did not change: only the peer re-queue brings it
		// back to TreeValid.
		waitFor(t, "a", metav1.ConditionTrue, v1alpha1.ReasonTreeValid)
		waitFor(t, "b", metav1.ConditionTrue, v1alpha1.ReasonTreeValid)
	})

	// The loop test took the webhook out, so this mob reaches the
	// reconciler, which refuses the image again.
	t.Run("reconciler refuses a mob image off the allow-list", mobs.imageReconciler)
}
