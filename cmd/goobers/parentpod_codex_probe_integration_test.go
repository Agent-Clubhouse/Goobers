//go:build integration && !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Exercise the same fixture the Kubernetes/daemon journeys install through the
// real preflight adapter, without needing a cluster or any model requests.
func TestIntegrationCodexQualificationHostPreflight(t *testing.T) {
	testdep.Require(t, "sh")
	t.Setenv("GOOBERS_PARENT_QUALIFICATION_HARNESS", "codex")
	bin := t.TempDir()
	installQualificationParentProbe(t, bin)
	command := filepath.Join(bin, "codex")
	adapter := &harness.CodexAdapter{
		Command:         []string{command},
		ModelCredential: func(context.Context) (string, error) { return "sk-qualification-model-only", nil },
	}
	info, err := adapter.Preflight(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Checks) == 0 || info.Checks[0].Outcome != "supported" {
		t.Fatalf("public help was not verified: %+v", info)
	}
	output, err := exec.CommandContext(t.Context(), command, "exec", "--json", "author a child").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "parent execution reached the host") {
		t.Fatalf("host inference was not refused: %v %s", err, output)
	}
}

func TestIntegrationCodexQualificationWorkerPreflight(t *testing.T) {
	testdep.Require(t, "node")
	data, err := os.ReadFile(filepath.Join("testdata", "qualification-claude.cjs"))
	if err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(command, data, 0700); err != nil {
		t.Fatal(err)
	}
	adapter := &harness.CodexAdapter{Command: []string{command}, ModelCredential: func(context.Context) (string, error) { return "sk-qualification-model-only", nil }}
	info, err := adapter.Preflight(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Checks) == 0 || info.Checks[0].Outcome != "supported" {
		t.Fatalf("worker help was not verified: %+v", info)
	}
}
