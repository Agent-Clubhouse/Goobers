package cliregistry

import (
	"strings"
	"testing"
)

func TestMatchPrefersLongestCommandPath(t *testing.T) {
	names := []string{"migrate", "migrate status", "runs list"}
	index, consumed, ok := Match(names, []string{"migrate", "status", "--api", "https://example.test"})
	if !ok || index != 1 || consumed != 2 {
		t.Fatalf("match = index %d consumed %d ok %v", index, consumed, ok)
	}
}

func TestCompletionUsesUniqueTopLevelCommands(t *testing.T) {
	got := Completion("goobers-cloud", "bash", []string{"runs list", "runs show", "status"})
	if strings.Count(got, "runs") != 1 || !strings.Contains(got, "status") {
		t.Fatalf("completion = %q", got)
	}
}
