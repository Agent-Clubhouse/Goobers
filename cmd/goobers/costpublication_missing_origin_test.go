package main

import "testing"

func TestCostPublicationEmptyRootCannotDiscardExplicitOrigin(t *testing.T) {
	t.Chdir(t.TempDir())
	if enabled, err := resolveCostPublication("", "", postMergeTestRepo()); !enabled || err != nil {
		t.Fatalf("unscoped legacy default changed: enabled=%v err=%v", enabled, err)
	}
	if enabled, err := resolveCostPublication("", "origin", postMergeTestRepo()); enabled || err == nil {
		t.Fatalf("explicit origin silently discarded: enabled=%v err=%v", enabled, err)
	}
}
