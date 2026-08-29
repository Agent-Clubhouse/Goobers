package tutorguard

import (
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestParseFindingMarkdownExtractsFrontMatter(t *testing.T) {
	data := []byte(`---
kind: gate-never-fails
subject: local-ci
independentProof: |
  Manual audit (run-482) confirms local-ci has been a no-op since #900 — the
  underlying binary it shells out to was removed in that PR.
---

## Finding

local-ci never fails across the last 200 runs.
`)
	meta, err := ParseFindingMarkdown(data)
	if err != nil {
		t.Fatalf("ParseFindingMarkdown: %v", err)
	}
	if meta.Kind != "gate-never-fails" {
		t.Errorf("Kind = %q, want gate-never-fails", meta.Kind)
	}
	if meta.Subject != "local-ci" {
		t.Errorf("Subject = %q, want local-ci", meta.Subject)
	}
	if !meta.HasIndependentProof() {
		t.Error("HasIndependentProof() = false, want true")
	}
	if !meta.IsGateNoise() {
		t.Error("IsGateNoise() = false, want true")
	}
}

func TestParseFindingMarkdownNoFrontMatterIsNotAnError(t *testing.T) {
	meta, err := ParseFindingMarkdown([]byte("## Finding\n\nJust prose, no header.\n"))
	if !errors.Is(err, ErrNoFrontMatter) {
		t.Fatalf("err = %v, want ErrNoFrontMatter", err)
	}
	if meta != (FindingMeta{}) {
		t.Fatalf("meta = %+v, want zero value", meta)
	}
}

func TestParseFindingMarkdownEmptyProofIsNotIndependentProof(t *testing.T) {
	meta, err := ParseFindingMarkdown([]byte("---\nkind: gate-never-fails\nsubject: local-ci\nindependentProof: \"   \"\n---\nbody\n"))
	if err != nil {
		t.Fatalf("ParseFindingMarkdown: %v", err)
	}
	if meta.HasIndependentProof() {
		t.Error("HasIndependentProof() = true for whitespace-only proof, want false")
	}
}

func TestFindingMetaIsGateNoise(t *testing.T) {
	cases := []struct {
		kind string
		want bool
	}{
		{"gate-never-fails", true},
		{"gate-repass-churn", true},
		{"stage-failure-rate", false},
		{"", false},
	}
	for _, c := range cases {
		if got := (FindingMeta{Kind: c.kind}).IsGateNoise(); got != c.want {
			t.Errorf("IsGateNoise(%q) = %v, want %v", c.kind, got, c.want)
		}
	}
}

const workflowFixture = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: example
spec:
  gaggle: goobers
  triggers:
    - type: manual
  start: build
  tasks:
    - name: build
      type: deterministic
      goal: build it
      run:
        command: ["true"]
      next: local-ci-gate
  gates:
    - name: local-ci-gate
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: done
        fail: "@abort"
`

func mustReplace(t *testing.T, old, from, to string) string {
	t.Helper()
	replaced := replaceOnce(old, from, to)
	if replaced == old {
		t.Fatalf("replaceOnce(%q -> %q) made no change", from, to)
	}
	return replaced
}

func replaceOnce(s, old, new string) string {
	i := indexOf(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestClassifyGateEditRemoved(t *testing.T) {
	newYAML := replaceOnce(workflowFixture, `  gates:
    - name: local-ci-gate
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: done
        fail: "@abort"
`, "  gates: []\n")
	kind, err := ClassifyGateEdit([]byte(workflowFixture), []byte(newYAML), "local-ci-gate")
	if err != nil {
		t.Fatalf("ClassifyGateEdit: %v", err)
	}
	if kind != GateEditRemoved {
		t.Fatalf("kind = %q, want removed", kind)
	}
	if !kind.RequiresIndependentProof() {
		t.Error("RequiresIndependentProof() = false for removed, want true")
	}
}

func TestClassifyGateEditLoosenedFailBranchRedirected(t *testing.T) {
	newYAML := mustReplace(t, workflowFixture, `fail: "@abort"`, `fail: done`)
	kind, err := ClassifyGateEdit([]byte(workflowFixture), []byte(newYAML), "local-ci-gate")
	if err != nil {
		t.Fatalf("ClassifyGateEdit: %v", err)
	}
	if kind != GateEditLoosened {
		t.Fatalf("kind = %q, want loosened", kind)
	}
	if !kind.RequiresIndependentProof() {
		t.Error("RequiresIndependentProof() = false for loosened, want true")
	}
}

func TestClassifyGateEditTuningUnrelatedFieldChange(t *testing.T) {
	newYAML := mustReplace(t, workflowFixture, "check: status-equals", "check: output-numeric-lte")
	kind, err := ClassifyGateEdit([]byte(workflowFixture), []byte(newYAML), "local-ci-gate")
	if err != nil {
		t.Fatalf("ClassifyGateEdit: %v", err)
	}
	if kind != GateEditTuning {
		t.Fatalf("kind = %q, want tuning", kind)
	}
	if kind.RequiresIndependentProof() {
		t.Error("RequiresIndependentProof() = true for tuning, want false")
	}
}

func TestClassifyGateEditNoneWhenUnchanged(t *testing.T) {
	kind, err := ClassifyGateEdit([]byte(workflowFixture), []byte(workflowFixture), "local-ci-gate")
	if err != nil {
		t.Fatalf("ClassifyGateEdit: %v", err)
	}
	if kind != GateEditNone {
		t.Fatalf("kind = %q, want none", kind)
	}
}

func TestClassifyGateEditNoneWhenGateAbsentFromBothRevisions(t *testing.T) {
	kind, err := ClassifyGateEdit([]byte(workflowFixture), []byte(workflowFixture), "some-other-gate")
	if err != nil {
		t.Fatalf("ClassifyGateEdit: %v", err)
	}
	if kind != GateEditNone {
		t.Fatalf("kind = %q, want none", kind)
	}
}

func TestClassifyGateEditNoneWhenFileAbsentAtBaseRevision(t *testing.T) {
	// A workflow file the run added fresh (not present at base) must never
	// read as "removed" — nil/empty old bytes model a nonexistent file.
	kind, err := ClassifyGateEdit(nil, []byte(workflowFixture), "local-ci-gate")
	if err != nil {
		t.Fatalf("ClassifyGateEdit: %v", err)
	}
	if kind != GateEditNone {
		t.Fatalf("kind = %q, want none", kind)
	}
}

func TestClassifyGateEditRejectsMalformedYAML(t *testing.T) {
	if _, err := ClassifyGateEdit([]byte("not: [valid"), []byte(workflowFixture), "local-ci-gate"); err == nil {
		t.Fatal("expected an error for malformed old YAML")
	}
	if _, err := ClassifyGateEdit([]byte(workflowFixture), []byte("not: [valid"), "local-ci-gate"); err == nil {
		t.Fatal("expected an error for malformed new YAML")
	}
}

// repassLoopWorkflowFixture mirrors the real shipped shape most likely to be
// named by a gate-repass-churn finding: a gate whose fail branch routes to a
// named repass/remediation state (implementation.yaml's `review` gate fails
// to "park-needs-human", `local-gate` fails to "implement"), never to the
// literal "@abort" terminal. A failsClosed() that only recognized "@abort"
// as blocking would report false for this gate even pre-edit, making the
// loosened-detection dead code for exactly this — the most common — shape.
const repassLoopWorkflowFixture = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: example
spec:
  gaggle: goobers
  triggers:
    - type: manual
  start: implement
  tasks:
    - name: implement
      type: deterministic
      goal: implement it
      run:
        command: ["true"]
      next: review-gate
    - name: push-branch
      type: deterministic
      goal: push it
      run:
        command: ["true"]
  gates:
    - name: review-gate
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: push-branch
        fail: implement
`

func TestClassifyGateEditLoosenedRepassLoopFailBranchConvergedWithPass(t *testing.T) {
	// The exact bypass QA/Dev-7 flagged: redirecting a repass-loop gate's
	// fail branch to converge with its own pass branch defeats the gate
	// (every outcome now proceeds) without ever touching "@abort" — this
	// must classify as loosened, not tuning.
	newYAML := mustReplace(t, repassLoopWorkflowFixture, "fail: implement", "fail: push-branch")
	kind, err := ClassifyGateEdit([]byte(repassLoopWorkflowFixture), []byte(newYAML), "review-gate")
	if err != nil {
		t.Fatalf("ClassifyGateEdit: %v", err)
	}
	if kind != GateEditLoosened {
		t.Fatalf("kind = %q, want loosened (repass-loop gate defeated by fail/pass convergence)", kind)
	}
	if !kind.RequiresIndependentProof() {
		t.Error("RequiresIndependentProof() = false for loosened, want true")
	}
}

func TestClassifyGateEditTuningRepassLoopUnrelatedFieldChange(t *testing.T) {
	// A repass-loop gate whose fail branch still differs from pass (still
	// blocking) after an unrelated field change is ordinary tuning, exactly
	// like the @abort-style gate case.
	newYAML := mustReplace(t, repassLoopWorkflowFixture, "check: status-equals", "check: output-numeric-lte")
	kind, err := ClassifyGateEdit([]byte(repassLoopWorkflowFixture), []byte(newYAML), "review-gate")
	if err != nil {
		t.Fatalf("ClassifyGateEdit: %v", err)
	}
	if kind != GateEditTuning {
		t.Fatalf("kind = %q, want tuning", kind)
	}
	if kind.RequiresIndependentProof() {
		t.Error("RequiresIndependentProof() = true for tuning, want false")
	}
}

func TestFailsClosedTrueForNonAbortRepassTarget(t *testing.T) {
	// Direct regression guard for the bug itself: a gate whose fail branch
	// names a repass/remediation state (not "@abort") must still read as
	// failing closed, since it blocks the happy path exactly as surely.
	g := apiv1.Gate{Branches: map[string]string{"pass": "push-branch", "fail": "implement"}}
	if !failsClosed(g) {
		t.Error("failsClosed() = false for a non-@abort but pass-distinct fail target, want true")
	}
}
