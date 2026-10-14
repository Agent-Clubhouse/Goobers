//go:build integration

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
	"gopkg.in/yaml.v3"
)

func TestIntegrationCodexParentAuthorsChildThroughRealWorkers(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	t.Setenv("GOOBERS_PARENT_QUALIFICATION_HARNESS", "codex")
	qualifyContainedParentJourney(t, "merge")
}

func TestIntegrationCodexParentCancellationStopsAuthoredChild(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	t.Setenv("GOOBERS_PARENT_QUALIFICATION_HARNESS", "codex")
	qualifyContainedParentJourney(t, "cancel")
}

func TestIntegrationContainedParentSurvivesDaemonProcessLossCodex(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	t.Setenv("GOOBERS_PARENT_QUALIFICATION_HARNESS", "codex")
	qualifyParentDaemonProcessLoss(t, false)
}

func qualificationParentHarness(t *testing.T) string {
	t.Helper()
	switch value := os.Getenv("GOOBERS_PARENT_QUALIFICATION_HARNESS"); value {
	case "", "claude-code":
		return "claude-code"
	case "codex":
		return value
	default:
		t.Fatal("unsupported qualification harness")
		return ""
	}
}

func installQualificationParentProbe(t *testing.T, bin string) {
	t.Helper()
	command := "claude"
	key := "sk-ant-qualification-model-only"
	if qualificationParentHarness(t) == "codex" {
		command, key = "codex", "sk-qualification-model-only"
	}
	// Only preflight may execute on the host. Inference must reach the pod.
	probe := "#!/bin/sh\ncase \"$*\" in\n --version) echo '2.1.0 (qualification preflight)' ;;\n 'auth status') echo '{\"loggedIn\":true}' ;;\n *) echo 'parent execution reached the host' >&2; exit 70 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, command), []byte(probe), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUALIFICATION_MODEL_TOKEN", key)
}

func qualificationParentGoober(t *testing.T, source, selectedHarness string) string {
	t.Helper()
	source = strings.Replace(source, "harness: copilot", "harness: "+selectedHarness, 1)
	if selectedHarness != "codex" {
		return source
	}
	// Codex does not support a built-in tool allowlist. Configure its supported
	// invocation shape; child admission and scoped MCP grants remain enforced.
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(source), &doc); err != nil {
		t.Fatal(err)
	}
	spec, ok := doc["spec"].(map[string]any)
	if !ok {
		t.Fatal("qualification Goober has no spec")
	}
	delete(spec, "tools")
	data, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
