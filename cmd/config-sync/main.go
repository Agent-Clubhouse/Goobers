// Command config-sync turns a config-as-code repo into the Goobers CRs the
// operator reconciles (M12). Default mode renders the desired CR manifest set
// for ArgoCD to apply; --apply writes directly to the cluster (and prunes).
//
//	config-sync --config ./my-config-repo --out ./rendered
//	config-sync --config ./my-config-repo --apply --namespace goobers-system
//
// Tier-3 (V2) — quarantined, not on the V0 path (the CRD-apply path; local
// tiers 1-2 watch config/ directly, see ARCHITECTURE.md §6). See
// docs/ARCHITECTURE.md §11. Revived in V2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/goobers/goobers/internal/configsync"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("config-sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configDir = fs.String("config", ".", "path to the config-as-code repo")
		outDir    = fs.String("out", "rendered", "output directory for rendered CR manifests")
		namespace = fs.String("namespace", configsync.DefaultNamespace, "target namespace for rendered CRs")
		apply     = fs.Bool("apply", false, "apply CRs directly to the cluster (and prune) instead of rendering")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	loader, err := configsync.NewLoader(*namespace)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "config-sync: %v\n", err)
		return 1
	}

	// Exclude the render output directory from the load so a config repo that
	// holds its own rendered/ output (the ArgoCD-watched layout) stays idempotent
	// across renders. Harmless when --out is outside the config root.
	set, report, err := loader.Load(*configDir, *outDir)
	if errors.Is(err, configsync.ErrInvalidConfig) {
		_, _ = fmt.Fprintf(stderr, "config-sync: invalid config (%d objects, %d files):\n", report.Objects, report.Files)
		for _, iss := range report.Issues {
			_, _ = fmt.Fprintf(stderr, "  %s\n", iss.CLIString())
		}
		return 1
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "config-sync: %v\n", err)
		return 1
	}
	// Surface non-fatal warnings.
	for _, iss := range report.Issues {
		_, _ = fmt.Fprintf(stderr, "  %s\n", iss.CLIString())
	}

	if *apply {
		if err := applyFn(context.Background(), set); err != nil {
			_, _ = fmt.Fprintf(stderr, "config-sync: apply failed: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "applied %d objects to namespace %s\n", len(set.Objects), set.Namespace)
		return 0
	}

	written, err := set.WriteManifests(*outDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "config-sync: render failed: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "rendered %d manifests to %s\n", len(written), *outDir)
	return 0
}

// applyFn performs the direct-apply path; overridable in tests so the apply
// branch is exercised without a live cluster.
var applyFn = applyToCluster

// applyToCluster builds an in-cluster client and applies the set directly.
func applyToCluster(ctx context.Context, set *configsync.RenderSet) error {
	scheme, err := configsync.NewScheme()
	if err != nil {
		return err
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load kube config: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}
	return (&configsync.ClientApplier{Client: c}).Apply(ctx, set)
}
