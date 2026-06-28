package operator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// Options configures the operator manager.
type Options struct {
	// MetricsAddr is the bind address for the metrics endpoint ("0" disables it).
	MetricsAddr string
	// HealthAddr is the bind address for health/readiness probes ("0" disables it).
	HealthAddr string
	// LeaderElection enables leader election for HA operator deployments.
	LeaderElection bool
}

// DefaultOptions returns sane defaults for running in-cluster.
func DefaultOptions() Options {
	return Options{MetricsAddr: ":8080", HealthAddr: ":8081", LeaderElection: false}
}

// Run builds the manager, wires the Gaggle reconciler, and blocks until ctx is
// cancelled. It is the body invoked from cmd/operator via internal/app.Main.
func Run(ctx context.Context, logger *slog.Logger, opts Options) error {
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

	scheme, err := NewScheme()
	if err != nil {
		return fmt.Errorf("build scheme: %w", err)
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load kube config: %w", err)
	}

	mgr, err := ctrl.NewManager(cfg, manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: opts.MetricsAddr},
		HealthProbeBindAddress: opts.HealthAddr,
		LeaderElection:         opts.LeaderElection,
		LeaderElectionID:       "goobers-operator.goobers.dev",
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz: %w", err)
	}

	reconciler := &GaggleReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Registrar: NoopRegistrar{Log: logger},
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup gaggle reconciler: %w", err)
	}

	logger.Info("operator manager starting")
	return mgr.Start(ctx)
}
