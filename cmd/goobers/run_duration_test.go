package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

func TestRunDurationTaskProcess(t *testing.T) {
	marker := os.Getenv("RUN_DURATION_TEST_MARKER")
	if marker == "" {
		return
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func initRunDurationDemo(t *testing.T) (root, marker string) {
	t.Helper()
	root = initDemo(t)
	marker = filepath.Join(root, "task-started")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	gaggleDir := filepath.Join(root, "config", "gaggles", "example")
	writeFixture(t, filepath.Join(gaggleDir, "workflows", "default-implement.yaml"), fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: manual
  start: probe
  tasks:
    - name: probe
      type: deterministic
      goal: Record whether execution started.
      run:
        workspace: scratch
        command: [%q, "-test.run=^TestRunDurationTaskProcess$"]
        env:
          RUN_DURATION_TEST_MARKER: %q
`, self, marker))
	if err := os.RemoveAll(filepath.Join(gaggleDir, "goobers")); err != nil {
		t.Fatal(err)
	}
	return root, marker
}

func setRunDurationLimit(t *testing.T, root, scope, duration string) {
	t.Helper()
	if scope == "instance" || scope == "repository" {
		path := instance.NewLayout(root).ConfigFile()
		cfg, err := instance.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if scope == "instance" {
			cfg.RunConditions.MaxRunDuration = duration
		} else {
			cfg.Repos[0].RunControls = &apiv1.RunControls{MaxRunDuration: duration}
		}
		if err := instance.WriteConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		return
	}
	path := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	if scope == "workflow" {
		path = filepath.Join(filepath.Dir(path), "workflows", "default-implement.yaml")
	} else if scope != "gaggle" {
		t.Fatalf("unknown run-control scope %q", scope)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, path, strings.Replace(string(body), "spec:\n", "spec:\n  runControls:\n    maxRunDuration: "+duration+"\n", 1))
}

func TestRunRejectsStandaloneMaxRunDurationBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope string
		args  []string
	}{
		{"workflow", "workflow", []string{"run", "--no-api", "default-implement"}},
		{"instance without no-api", "instance", []string{"run", "default-implement"}},
		{"repository with force", "repository", []string{"run", "--force", "default-implement"}},
		{"gaggle with qualified workflow", "gaggle", []string{"run", "example/default-implement"}},
		{"no-wait", "workflow", []string{"run", "--no-api", "--no-wait", "default-implement"}},
		{"detached worker", "workflow", []string{detachedRunWorkerCommand, "default-implement"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, marker := initRunDurationDemo(t)
			setRunDurationLimit(t, root, tc.scope, "3s")
			if code, _, stderr := runArgs(t, "validate", root); code != 0 {
				t.Fatalf("validate: code=%d stderr=%q", code, stderr)
			}

			code, stdout, stderr := runArgs(t, append(tc.args, root)...)
			if code != 1 {
				t.Errorf("run: code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
			}
			for _, want := range []string{"maxRunDuration", "3s", "standalone", "goobers up"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr=%q, want %q", stderr, want)
				}
			}
			if strings.Contains(stdout, "created run ") {
				t.Errorf("rejected run reported dispatch: %q", stdout)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Errorf("task ran despite rejection: marker stat=%v", err)
			}
			entries, err := os.ReadDir(instance.NewLayout(root).ForGaggle("example").RunsDir())
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("created %d run journals before rejection", len(entries))
			}
		})
	}
}

func TestRunStandaloneWithoutMaxRunDurationCompletes(t *testing.T) {
	root, marker := initRunDurationDemo(t)
	code, stdout, stderr := runArgs(t, "run", "--no-api", "default-implement", root)
	if code != 0 || !strings.Contains(stdout, "phase=completed") {
		t.Fatalf("run: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("task did not execute: %v", err)
	}
}

func TestRunStandaloneDurationLimitUsesSelectedWorkflow(t *testing.T) {
	root, _ := initRunDurationDemo(t)
	installSecondDaemonGaggle(t, root)
	setRunDurationLimit(t, root, "workflow", "3s")

	code, stdout, stderr := runArgs(t, "run", "--no-api", "deploy", root)
	if code != 1 || !strings.Contains(stderr, "ambiguous") {
		t.Fatalf("ambiguous run: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr = runArgs(t, "run", "--no-api", "beta/deploy", root)
	if code != 0 || !strings.Contains(stdout, "phase=completed") {
		t.Fatalf("unlimited workflow: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr = runArgs(t, "run", "--no-api", "--gaggle", "example", "deploy", root)
	if code != 1 || !strings.Contains(stderr, "maxRunDuration=3s") {
		t.Fatalf("limited workflow: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if count := targetedRunCount(t, root); count != 0 {
		t.Errorf("created %d run journals for the rejected workflow", count)
	}
}
