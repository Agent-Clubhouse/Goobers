package runtimewiring

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// DeterministicStageConfigDigest hashes the config tree used by deterministic stages.
func DeterministicStageConfigDigest(configDir, gaggle string) (string, error) {
	digest, err := ConfigDirectoryDigestForGaggle(configDir, gaggle)
	if err != nil {
		return "", fmt.Errorf("digest deterministic-stage config: %w", err)
	}
	return digest, nil
}

// KitModeFor selects the verified completion contract for a stage kit.
func KitModeFor(attempt dispatcher.Attempt) agentickit.Mode {
	if attempt.Review {
		return agentickit.ModeReview
	}
	return agentickit.ModeInvoke
}

// GaggleNamespacesFromConfig requires an explicit dispatch namespace for every gaggle.
func GaggleNamespacesFromConfig(gaggles []apiv1.Gaggle) (map[string]string, error) {
	namespaces := make(map[string]string, len(gaggles))
	for i := range gaggles {
		gaggle := &gaggles[i]
		namespace := strings.TrimSpace(gaggle.Spec.Isolation.Namespace)
		if namespace == "" {
			return nil, fmt.Errorf("stage dispatch: gaggle %q declares no isolation.namespace", gaggle.Name)
		}
		namespaces[gaggle.Name] = namespace
	}
	return namespaces, nil
}

// ValidateSeededWorkerConfig validates a worker seed without fetching or publishing config.
func ValidateSeededWorkerConfig(root string) error {
	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return err
	}
	if config.WorkflowSource != nil || config.ConfigMirrorPath != "" {
		return errors.New("worker seed must not fetch config sources or publish mirrors")
	}
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		return fmt.Errorf("seeded worker config is invalid: %w (%s)", err, ValidationIssueSummary(report))
	}
	// Parsing YAML alone is insufficient: first dispatch also needs every
	// referenced instruction body and skill package, including large prose.
	_, _, err = LoadSnapshotGooberInputs(layout.ConfigDir(), set)
	if err != nil {
		return err
	}
	// The digest loader also verifies asset bytes against preserved source
	// modes, including when an init-container retry reuses a completed seed.
	_, err = ConfigDirectoryDigest(layout.ConfigDir())
	return err
}

// PrepareManualRoot validates and displays the mutation target, adopting legacy identity when needed.
func PrepareManualRoot(layout instance.Layout, diagnostic io.Writer) error {
	if err := ValidateStartupRoot(layout); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := instance.EnsureRootIdentity(ctx, layout.Root)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(diagnostic, "Instance root: %q; instance ID: %q\n", CanonicalStatusRoot(layout.Root), id); err != nil {
		return fmt.Errorf("display mutation target: %w", err)
	}
	return nil
}

// DivergenceRunnerString reads a string field from a divergence event.
func DivergenceRunnerString(event journal.Event, key string) string {
	value, _ := event.Runner[key].(string)
	return value
}

// ValidateStartupRoot requires an initialized, current instance root.
func ValidateStartupRoot(layout instance.Layout) error {
	if _, err := os.Stat(layout.ConfigFile()); err != nil {
		return fmt.Errorf("%s not found (not an instance root — run `goobers init` first)", layout.ConfigFile())
	}
	return instance.RequireCurrentRoot(layout.Root)
}

// CanonicalStatusRoot resolves an instance path, including symlinks when available.
func CanonicalStatusRoot(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	return absolute
}
