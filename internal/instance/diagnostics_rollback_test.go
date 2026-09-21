//go:build rollbackcompat

package instance

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run only in scripts/test-diagnostics-rollback.sh, on either side
// of the real v0.4.1 reader. No downgraded decoder is reimplemented here.
func TestDiagnosticsRollbackConfigPrepare(t *testing.T) {
	root := rollbackConfigRoot(t)
	rollbackClearOTLPEnvironment(t)
	before := []byte("apiVersion: goobers.dev/v1alpha1\nkind: Instance\nselfIdentity: rollback-bot\nrunConditions:\n  maxParallelRuns: 2\n")
	rollbackWriteConfigFixture(t, filepath.Join(root, "before.yaml"), before)
	for _, name := range []string{"diagnostics-on", "diagnostics-disabled", "journal-disabled", "both-disabled", "endpoint-only"} {
		cfg, err := LoadConfig(filepath.Join(root, "before.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		disabled := false
		if name == "diagnostics-on" || name == "diagnostics-disabled" || name == "both-disabled" {
			cfg.Telemetry.Diagnostics = &DiagnosticsConfig{Organization: "rollback-fixture", OTLP: &OTLPConfig{Endpoint: "https://diagnostics.example.test:4317"}}
			if name != "diagnostics-on" {
				cfg.Telemetry.Diagnostics.OTLP.ExportEnabled = &disabled
			}
		}
		if name == "journal-disabled" || name == "both-disabled" || name == "endpoint-only" {
			cfg.Telemetry.OTLP = &OTLPConfig{Endpoint: "https://journal.example.test:4317"}
			if name != "endpoint-only" {
				cfg.Telemetry.OTLP.ExportEnabled = &disabled
			}
		}
		path := filepath.Join(root, name+".yaml")
		if err := WriteConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("current reader rejects %s: %v", name, err)
		}
		if loaded.DiagnosticOTLP().Enabled() != (name == "diagnostics-on") {
			t.Fatalf("current diagnostic opt-in changed for %s", name)
		}
		if (loaded.Telemetry.OTLP != nil && loaded.Telemetry.OTLP.Enabled()) != (name == "endpoint-only") {
			t.Fatalf("current journal opt-out ignored for %s", name)
		}
	}
	for _, name := range []string{"definitions-before", "gaggle-paused", "workflow-paused"} {
		dir := filepath.Join(root, name)
		gaggle, workflow := rollbackGaggle, rollbackWorkflow
		if name == "gaggle-paused" {
			gaggle = strings.Replace(gaggle, "spec:\n", "spec:\n  enabled: false\n", 1)
		}
		if name == "workflow-paused" {
			workflow = strings.Replace(workflow, "spec:\n", "spec:\n  enabled: false\n", 1)
		}
		rollbackWriteConfigFixture(t, filepath.Join(dir, "manifest.yaml"), []byte(rollbackManifest))
		rollbackWriteConfigFixture(t, filepath.Join(dir, "gaggles/example/gaggle.yaml"), []byte(gaggle))
		rollbackWriteConfigFixture(t, filepath.Join(dir, "gaggles/example/workflows/manual.yaml"), []byte(workflow))
		set, report, err := LoadConfigDir(dir)
		if err != nil {
			t.Fatalf("current definitions %s: %v (%+v)", name, err, report)
		}
		if len(set.Gaggles) != 1 || len(set.Workflows) != 1 {
			t.Fatal("fixture must contain exactly one gaggle and manual workflow")
		}
		if name == "gaggle-paused" && (set.Gaggles[0].Spec.Enabled == nil || *set.Gaggles[0].Spec.Enabled) {
			t.Fatal("current gaggle pause was lost")
		}
		if name == "workflow-paused" && (set.Workflows[0].Spec.Enabled == nil || *set.Workflows[0].Spec.Enabled) {
			t.Fatal("current workflow pause was lost")
		}
	}
}

func TestDiagnosticsRollbackConfigVerify(t *testing.T) {
	root := rollbackConfigRoot(t)
	rollbackClearOTLPEnvironment(t)
	before, err := os.ReadFile(filepath.Join(root, "before.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(filepath.Join(root, "restored-instance.yaml"))
	if err != nil || !bytes.Equal(before, restored) {
		t.Fatalf("prior reader did not restore exact pre-upgrade instance config: %v", err)
	}
	cfg, err := LoadConfig(filepath.Join(root, "restored-instance.yaml"))
	if err != nil || cfg.Telemetry.Diagnostics != nil || (cfg.Telemetry.OTLP != nil && cfg.Telemetry.OTLP.Enabled()) {
		t.Fatalf("restored configuration changed on re-upgrade: %v", err)
	}
}

func rollbackConfigRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv("GOOBERS_ROLLBACK_FIXTURE")
	if root == "" {
		t.Fatal("GOOBERS_ROLLBACK_FIXTURE must name the shared rollback fixture")
	}
	return filepath.Join(root, "config")
}

func rollbackClearOTLPEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{OTLPEndpointEnv, OTLPInsecureEnv} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

func rollbackWriteConfigFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

const rollbackManifest = `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: rollback
spec:
  instance:
    name: rollback
    environment: dev
  gaggles: [example]
`
const rollbackGaggle = `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: example
spec:
  project:
    provider: github
    owner: acme
    name: web
  backlog:
    provider: github
    project: acme/web
  isolation:
    namespace: gaggle-example
`
const rollbackWorkflow = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: manual
spec:
  gaggle: example
  triggers:
    - type: manual
  start: check
  tasks:
    - name: check
      type: deterministic
      goal: Check the restored configuration without automatic admission.
      run:
        command: ["true"]
`
