package main

import (
	"testing"
)

func TestBranchPushReceiptsHaveDistinctDurableIdentities(t *testing.T) {
	dir := t.TempDir()
	const branch = "goobers/implementation/run"
	appendBranchPushFact(dir, branch)
	appendBranchPushFact(dir, branch)
	facts := readMutationFacts(t, dir)
	if len(facts) != 2 {
		t.Fatalf("push receipts = %d, want 2", len(facts))
	}
	for _, fact := range facts {
		if fact.ReceiptID == "" || fact.Provider != "git" || fact.Kind != "branch" || fact.ID != branch || fact.Operation != "push" {
			t.Fatalf("invalid push receipt: %+v", fact)
		}
	}
	if facts[0].ReceiptID == facts[1].ReceiptID {
		t.Fatal("separate pushes reused a receipt identity")
	}
}
