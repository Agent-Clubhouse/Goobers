package main

import (
	"strings"
	"testing"
)

func TestRunTargetRejectsExplicitZeroPR(t *testing.T) {
	if !flagWasSet([]string{"merge-review", "--pr", "0"}, "pr") {
		t.Fatal("flagWasSet did not detect an explicit --pr flag")
	}
}

func TestRunTargetAllowsQualifiedMergeReview(t *testing.T) {
	target, err := parseRunTarget("release/merge-review", "")
	if err != nil {
		t.Fatalf("parseRunTarget: %v", err)
	}
	if target.Gaggle != "release" || target.Workflow != "merge-review" {
		t.Fatalf("target = %+v", target)
	}
}

func TestRunTargetCLIRejectsInvalidTargetedPullRequests(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "zero", args: []string{"run", "merge-review", "--pr", "0", t.TempDir()}},
		{name: "negative", args: []string{"run", "merge-review", "--pr", "-1", t.TempDir()}},
		{name: "wrong workflow", args: []string{"run", "implement", "--pr", "42", t.TempDir()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, _, stderr := runArgs(t, test.args...)
			if code != 2 {
				t.Fatalf("code = %d, want usage error 2; stderr = %q", code, stderr)
			}
			if !strings.Contains(stderr, "positive pull request number") {
				t.Fatalf("stderr = %q, want actionable --pr error", stderr)
			}
		})
	}
}
