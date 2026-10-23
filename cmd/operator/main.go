// Command operator is the Goobers Kubernetes operator. It reconciles Goobers
// CRDs (from /api) into running goober replicas and Temporal workflow
// registrations (DEP-012).
//
// Tier-3 (V2) — quarantined, not on the V0 path. See docs/ARCHITECTURE.md §11.
// Revived in V2.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/goobers/goobers/internal/app"
	"github.com/goobers/goobers/internal/operator"
)

func main() {
	app.Main("operator", func(ctx context.Context, log *slog.Logger) error {
		opts, err := optionsFromEnvironment(os.Getenv)
		if err != nil {
			return err
		}
		return operator.Run(ctx, log, opts)
	})
}

func optionsFromEnvironment(getenv func(string) string) (operator.Options, error) {
	opts := operator.DefaultOptions()
	if value := getenv("GOOBERS_OPERATOR_METRICS_ADDR"); value != "" {
		opts.MetricsAddr = value
	}
	if value := getenv("GOOBERS_OPERATOR_HEALTH_ADDR"); value != "" {
		opts.HealthAddr = value
	}
	if value := getenv("GOOBERS_OPERATOR_WORKER_IMAGE"); value != "" {
		opts.WorkerImage = value
	}
	if value := getenv("GOOBERS_OPERATOR_LEADER_ELECTION"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return operator.Options{}, fmt.Errorf("GOOBERS_OPERATOR_LEADER_ELECTION must be a boolean: %w", err)
		}
		opts.LeaderElection = enabled
	}
	return opts, nil
}
