package main

import "testing"

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
