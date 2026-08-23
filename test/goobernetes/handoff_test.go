package goobernetes

import "testing"

func TestAssertDeclaredEdgeHandoffPass(t *testing.T) {
	coverage := map[string][]string{"local-ci": {"implement"}}
	evidence := RepoHandoffEvidence{
		Producer: "implement", Consumer: "local-ci",
		ProducerNode: "node-a", ConsumerNode: "node-b",
		PushedSHA: "abc123", CheckedOutSHA: "abc123",
	}
	got := AssertDeclaredEdgeHandoff(coverage, evidence)
	if got.Verdict != VerdictPass {
		t.Fatalf("Verdict = %v, want pass; detail=%q", got.Verdict, got.Detail)
	}
}

func TestAssertDeclaredEdgeHandoffFailsOnUndeclaredEdge(t *testing.T) {
	coverage := map[string][]string{"local-ci": {"some-other-stage"}}
	evidence := RepoHandoffEvidence{
		Producer: "implement", Consumer: "local-ci",
		ProducerNode: "node-a", ConsumerNode: "node-b",
		PushedSHA: "abc123", CheckedOutSHA: "abc123",
	}
	got := AssertDeclaredEdgeHandoff(coverage, evidence)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (edge not declared)", got.Verdict)
	}
}

func TestAssertDeclaredEdgeHandoffFailsOnSameNode(t *testing.T) {
	coverage := map[string][]string{"local-ci": {"implement"}}
	evidence := RepoHandoffEvidence{
		Producer: "implement", Consumer: "local-ci",
		ProducerNode: "node-a", ConsumerNode: "node-a",
		PushedSHA: "abc123", CheckedOutSHA: "abc123",
	}
	got := AssertDeclaredEdgeHandoff(coverage, evidence)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (same node)", got.Verdict)
	}
}

// TestAssertDeclaredEdgeHandoffFailsOnSHADiscontinuity is S3's headline
// case: "the silent-worktree-continuity assumption is exactly what this
// criterion exists to falsify."
func TestAssertDeclaredEdgeHandoffFailsOnSHADiscontinuity(t *testing.T) {
	coverage := map[string][]string{"local-ci": {"implement"}}
	evidence := RepoHandoffEvidence{
		Producer: "implement", Consumer: "local-ci",
		ProducerNode: "node-a", ConsumerNode: "node-b",
		PushedSHA: "abc123", CheckedOutSHA: "def456",
	}
	got := AssertDeclaredEdgeHandoff(coverage, evidence)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (SHA discontinuity)", got.Verdict)
	}
}

func TestAssertDeclaredEdgeHandoffInvalidOnMissingSHA(t *testing.T) {
	coverage := map[string][]string{"local-ci": {"implement"}}
	evidence := RepoHandoffEvidence{
		Producer: "implement", Consumer: "local-ci",
		ProducerNode: "node-a", ConsumerNode: "node-b",
	}
	got := AssertDeclaredEdgeHandoff(coverage, evidence)
	if got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid (no SHA recorded — the runtime continuity observer is topology-pending)", got.Verdict)
	}
}

func TestAssertDeclaredEdgeHandoffInvalidWithNoCoverage(t *testing.T) {
	got := AssertDeclaredEdgeHandoff(nil, RepoHandoffEvidence{Producer: "a", Consumer: "b"})
	if got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid", got.Verdict)
	}
}
