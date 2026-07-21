package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

const invalidElectLanderWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: schedule
      schedule: "@every 24h"
  start: elect-lander
  tasks:
    - name: elect-lander
      type: deterministic
      goal: Elect the next pull request to land.
      run:
        command: ["goobers", "elect-lander"]
      inputs:
        selectedNumber: "42"
      expectedOutputs:
        - elected
        - reviewDigest
      next: apply-verdict
    - name: apply-verdict
      type: deterministic
      goal: Apply the election result.
      run:
        command: ["true"]
      inputsFrom:
        elected: elected
        reviewDigest: reviewDigest
`

func installInvalidElectLanderWorkflow(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(path, []byte(invalidElectLanderWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUpPreflightRejectsElectLanderDefectBeforeSchedulerState(t *testing.T) {
	root := initDeterministicDemo(t)
	installInvalidElectLanderWorkflow(t, root)
	layout := instance.NewLayout(root)
	if err := os.RemoveAll(layout.SchedulerDir()); err != nil {
		t.Fatal(err)
	}

	validateCode, validateOut, validateErr := runArgs(t, "validate", root)
	if validateCode != 1 {
		t.Fatalf("validate code = %d, want 1; stdout = %q, stderr = %q", validateCode, validateOut, validateErr)
	}

	var stdout, stderr bytes.Buffer
	if code := runUpContext(context.Background(), []string{root}, &stdout, &stderr); code != 1 {
		t.Fatalf("up code = %d, want 1; stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if got, want := stderr.String(), validateOut+validateErr; got != want {
		t.Fatalf("up validation output differs from validate:\nup:       %q\nvalidate: %q", got, want)
	}
	if !strings.Contains(stderr.String(), `task "elect-lander"`) {
		t.Fatalf("up stderr does not name elect-lander: %q", stderr.String())
	}
	if _, err := os.Stat(layout.SchedulerDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scheduler state was created before preflight refusal: %v", err)
	}
}

func TestUpSkipPreflightStartsWithNamedValidationWarning(t *testing.T) {
	root := initDeterministicDemo(t)
	installInvalidElectLanderWorkflow(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr bytes.Buffer
	if code := runUpContext(ctx, []string{"--quiet", "--skip-preflight", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("up code = %d, want 0; stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"WARNING: --skip-preflight enabled",
		`task "elect-lander"`,
		"no inputs.resultFile",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("up stderr missing %q: %q", want, stderr.String())
		}
	}
}
