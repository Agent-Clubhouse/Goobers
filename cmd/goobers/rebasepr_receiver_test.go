package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type fixtureGitTrace struct {
	Event string   `json:"event"`
	SID   string   `json:"sid"`
	Name  string   `json:"name"`
	Argv  []string `json:"argv"`
}

// Git's local transport clears both -c and GIT_CONFIG_* before receive-pack.
// Inspect its actual children rather than the suite's already-passing client
// config guard, and never try to win a race against background maintenance.
func TestAdjacentConflictFixtureReceiverCannotSpawnMaintenance(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "git-trace.jsonl")
	t.Setenv("GIT_TRACE2_EVENT", trace)
	origin := initAdjacentConflictPRBranch(t, "goobers/impl/receiver-guard", "[id].tsx", "base\n", "base\npr\n", "base\nmain\n", "")
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	var events []fixtureGitTrace
	receivers := make(map[string]bool)
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event fixtureGitTrace
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("invalid Git trace: %v", err)
		}
		events = append(events, event)
		if event.Event == "cmd_name" && event.Name == "receive-pack" {
			receivers[event.SID] = true
		}
	}
	if len(receivers) != 3 {
		t.Fatalf("observed %d receivers, want all three real fixture pushes", len(receivers))
	}
	for _, event := range events {
		if !receivers[event.SID] || event.Event != "child_start" || len(event.Argv) < 2 {
			continue
		}
		if event.Argv[1] == "maintenance" || event.Argv[1] == "gc" {
			t.Fatalf("fixture receiver spawned housekeeping after caller config was stripped: %v", event.Argv)
		}
	}
	// Keep the origin usable by the exact production provisioning path that
	// raced a disappearing .tmp-pack index in the reported coverage failure.
	wt := prWorktree(t, origin, "goobers/impl/receiver-guard")
	if _, err := os.Stat(filepath.Join(wt.Path, "[id].tsx")); err != nil {
		t.Fatal(err)
	}
}
