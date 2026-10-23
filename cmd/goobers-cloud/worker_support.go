package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

type executionFenceStart func(context.Context, v1alpha1.InvocationEnvelope) (context.Context, context.CancelFunc, error)

var loadConfigDirectory = instance.LoadConfigDir
var diagnosticsMode bool
var harnessAdapterFor = adapterFor

const diagnosticsMaxOutputBytes int64 = 64 << 20
const harnessPreflightTimeout = 180 * time.Second

var copilotAuthCheckArgs = []string{"-p", "Reply with exactly: ok", "--allow-all-tools", "--available-tools="}

const copilotAuthCheckSuccessLine = "ok"

func buildAutomationDeterministicExecutor(input deterministicExecutorInput) (invoke.Deterministic, error) {
	return buildDeterministicExecutor(input)
}

func trustedLocalChildren(*instance.Config) bool { return false }

func localSharedExecutionFence(instance.Layout) executionFenceStart {
	return func(ctx context.Context, _ v1alpha1.InvocationEnvelope) (context.Context, context.CancelFunc, error) {
		return ctx, func() {}, nil
	}
}

type teeRegistrar struct {
	run    runner.SecretRegistrar
	shared *journal.RegistryScrubber
}

func (t teeRegistrar) Register(secret []byte) {
	if t.run != nil {
		t.run.Register(secret)
	}
	if t.shared != nil {
		t.shared.Register(secret)
	}
}

func (t teeRegistrar) RegisterUntil(secret []byte, expiresAt time.Time) {
	if t.run != nil {
		t.run.Register(secret)
	}
	if t.shared != nil {
		t.shared.RegisterUntil(secret, expiresAt)
	}
}

func adapterFor(h v1alpha1.Harness, environment harness.EnvironmentConfig, harnessCommand map[string][]string, modelCredential func(context.Context) (string, error)) (harness.Adapter, error) {
	registry, err := buildHarnessRegistry(nil, environment, harnessCommand, "", "", false, modelCredential, false)
	if err != nil {
		return nil, err
	}
	if h == "" {
		h = v1alpha1.HarnessCopilot
	}
	return registry.Get(string(h))
}

func prepareManualRoot(layout instance.Layout, diagnostic io.Writer) error {
	if _, err := os.Stat(layout.ConfigFile()); err != nil {
		return fmt.Errorf("%s not found (not an instance root)", layout.ConfigFile())
	}
	if err := instance.RequireCurrentRoot(layout.Root); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := instance.EnsureRootIdentity(ctx, layout.Root)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(layout.Root)
	if err != nil {
		absolute = filepath.Clean(layout.Root)
	}
	_, err = fmt.Fprintf(diagnostic, "Instance root: %q; instance ID: %q\n", absolute, id)
	return err
}

func validationIssueSummary(report *validate.Report) string {
	if report == nil {
		return ""
	}
	var messages []string
	for _, issue := range report.Issues {
		if issue.Severity == validate.Error {
			messages = append(messages, issue.String())
		}
	}
	sort.Strings(messages)
	return fmt.Sprint(messages)
}

func configDirectoryDigest(root string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%d\x00", filepath.ToSlash(relative), len(content))
		_, _ = hash.Write(content)
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}

func configDirectoryDigestForGaggle(root, _ string) (string, error) {
	return configDirectoryDigest(root)
}

func validateSeededWorkerConfig(root string) error {
	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return err
	}
	if config.WorkflowSource != nil || config.ConfigMirrorPath != "" {
		return fmt.Errorf("worker seed must not fetch config sources or publish mirrors")
	}
	set, report, err := loadConfigDirectory(layout.ConfigDir())
	if err != nil {
		return fmt.Errorf("seeded worker config is invalid: %w (%s)", err, validationIssueSummary(report))
	}
	if _, _, err := loadSnapshotGooberInputs(layout.ConfigDir(), set); err != nil {
		return err
	}
	_, err = configDirectoryDigest(layout.ConfigDir())
	return err
}
