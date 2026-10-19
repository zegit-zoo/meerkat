// Command meerkat-operator runs the meerkat mob operator
// (meerkat-mob#10, phase 2): the validating webhook for KnowledgeBase
// and MeerkatMob objects, and the KnowledgeBase reconciler, which
// re-checks each tree after admission and reports it in status.
//
// It is not released yet: .goreleaser.yaml and the image build only
// ship cmd/meerkat and cmd/meerkat-bootstrap.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
	"github.com/zegit-zoo/meerkat/internal/operator/controller"
	opwebhook "github.com/zegit-zoo/meerkat/internal/operator/webhook"
)

// Leader election takes a Lease, and records events about it.
//
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Set via -X ldflags at build time; "dev" for go build/go test.
var version = "dev"

// options are the operator's command-line flags.
type options struct {
	metricsAddr    string
	probeAddr      string
	leaderElect    bool
	webhooks       bool
	webhookPort    int
	webhookCertDir string
}

// parseFlags reads args (without the program name).
func parseFlags(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("meerkat-operator", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.metricsAddr, "metrics-bind-address", "0", `address for the metrics endpoint, or "0" to turn it off`)
	fs.StringVar(&o.probeAddr, "health-probe-bind-address", ":8081", "address for the /healthz and /readyz probes")
	fs.BoolVar(&o.leaderElect, "leader-elect", false, "elect a leader, so only one replica reconciles at a time")
	fs.BoolVar(&o.webhooks, "webhooks", true, "serve the validating webhook (needs a serving certificate in --webhook-cert-dir)")
	fs.IntVar(&o.webhookPort, "webhook-port", 9443, "port the webhook server listens on")
	fs.StringVar(&o.webhookCertDir, "webhook-cert-dir", "", "directory holding tls.crt and tls.key for the webhook (default: controller-runtime's)")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if *showVersion {
		return o, errVersion
	}
	return o, nil
}

// errVersion asks main to print the version and exit 0.
var errVersion = errors.New("version requested")

// newScheme registers the types the operator reads and writes.
func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

func run(o options) error {
	log := logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, nil))
	ctrl.SetLogger(log)

	scheme, err := newScheme()
	if err != nil {
		return fmt.Errorf("building the scheme: %w", err)
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("loading the kubeconfig: %w", err)
	}
	mopts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: o.metricsAddr},
		HealthProbeBindAddress: o.probeAddr,
		LeaderElection:         o.leaderElect,
		LeaderElectionID:       "meerkat-operator.meerkat-mob.dev",
	}
	if o.webhooks {
		mopts.WebhookServer = webhook.NewServer(webhook.Options{Port: o.webhookPort, CertDir: o.webhookCertDir})
	}
	mgr, err := ctrl.NewManager(cfg, mopts)
	if err != nil {
		return fmt.Errorf("creating the manager: %w", err)
	}
	if err := (&controller.KnowledgeBaseReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("KnowledgeBase controller: %w", err)
	}
	if o.webhooks {
		if err := opwebhook.Setup(mgr); err != nil {
			return err
		}
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("healthz check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("readyz check: %w", err)
	}
	log.Info("starting meerkat-operator", "version", version, "webhooks", o.webhooks)
	return mgr.Start(ctrl.SetupSignalHandler())
}

func main() {
	o, err := parseFlags(os.Args[1:], os.Stderr)
	switch {
	case errors.Is(err, errVersion):
		fmt.Println("meerkat-operator", version)
		return
	case errors.Is(err, flag.ErrHelp):
		return
	case err != nil:
		os.Exit(2)
	}
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "meerkat-operator:", err)
		os.Exit(1)
	}
}
