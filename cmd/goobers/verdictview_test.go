package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func TestTraceVerdictsCoversGateDecisionsAndEscalation(t *testing.T) {
	tests := []struct {
		name     string
		decision apiv1.VerdictDecision
		target   string
	}{
		{name: "pass", decision: apiv1.VerdictPass, target: workflow.TerminalComplete},
		{name: "needs-changes", decision: apiv1.VerdictNeedsChanges, target: "implement"},
		{name: "fail", decision: apiv1.VerdictFail, target: workflow.TargetAbort},
		{name: "escalate", decision: apiv1.VerdictNeedsChanges, target: workflow.TargetEscalate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			runID := "verdict-" + tt.name
			recordTraceVerdict(t, root, runID, tt.decision, tt.target, apiv1.Verdict{
				Decision:  tt.decision,
				Rationale: "review rationale for " + tt.name,
				Findings: []apiv1.Finding{{
					Severity: apiv1.SeverityError,
					Location: "widget.go:42",
					Message:  "fix the widget",
				}},
			}, true, "sha256:reviewed-diff")

			code, stdout, stderr := runArgs(t, "trace", "--verdicts", runID, root)
			if code != 0 {
				t.Fatalf("trace --verdicts: code = %d, stderr = %q", code, stderr)
			}
			for _, want := range []string{
				"gate=review decision=" + string(tt.decision) + " target=" + tt.target + " cached=true",
				"diffDigest=sha256:reviewed-diff",
				"rationale: review rationale for " + tt.name,
				"severity=error location=widget.go:42 message=fix the widget",
			} {
				if !strings.Contains(stdout, want) {
					t.Errorf("trace --verdicts missing %q:\n%s", want, stdout)
				}
			}

			code, stdout, stderr = runArgs(t, "trace", "--json", runID, root)
			if code != 0 {
				t.Fatalf("trace --json: code = %d, stderr = %q", code, stderr)
			}
			var got traceJSONResult
			if err := json.Unmarshal([]byte(stdout), &got); err != nil {
				t.Fatalf("decode trace JSON: %v", err)
			}
			if len(got.Verdicts) != 1 {
				t.Fatalf("verdicts = %+v", got.Verdicts)
			}
			verdict := got.Verdicts[0]
			if verdict.Gate != "review" || verdict.Decision != string(tt.decision) ||
				verdict.Target != tt.target || !verdict.Cached ||
				verdict.DiffDigest != "sha256:reviewed-diff" ||
				verdict.Content == nil || verdict.Content.Rationale != "review rationale for "+tt.name ||
				len(verdict.Findings) != 1 {
				t.Fatalf("verdict = %+v", verdict)
			}
		})
	}
}

func TestTraceVerdictHumanOutputIsBoundedButJSONIsComplete(t *testing.T) {
	root := t.TempDir()
	const runID = "large-verdict"
	rationale := strings.Repeat("r", verdictHumanRationaleLimit) + "RATIONALE-TAIL"
	message := strings.Repeat("m", verdictHumanMessageLimit) + "MESSAGE-TAIL"
	recordTraceVerdict(t, root, runID, apiv1.VerdictNeedsChanges, "implement", apiv1.Verdict{
		Decision:  apiv1.VerdictNeedsChanges,
		Rationale: rationale,
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityWarning,
			Message:  message,
		}},
	}, false, "")

	code, stdout, stderr := runArgs(t, "trace", "--summary", runID, root)
	if code != 0 {
		t.Fatalf("trace --summary: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "[truncated]") ||
		strings.Contains(stdout, "RATIONALE-TAIL") ||
		strings.Contains(stdout, "MESSAGE-TAIL") {
		t.Fatalf("human verdict was not bounded:\n%s", stdout)
	}

	code, stdout, stderr = runArgs(t, "trace", "--json", runID, root)
	if code != 0 {
		t.Fatalf("trace --json: code = %d, stderr = %q", code, stderr)
	}
	var got traceJSONResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode trace JSON: %v", err)
	}
	if len(got.Verdicts) != 1 || got.Verdicts[0].Content == nil ||
		got.Verdicts[0].Content.Rationale != rationale ||
		got.Verdicts[0].Content.Findings[0].Message != message {
		t.Fatalf("JSON verdict was truncated: %+v", got.Verdicts)
	}
}

func TestTraceVerdictsReportsMissingArtifact(t *testing.T) {
	root := t.TempDir()
	const runID = "missing-verdict"
	ref := recordTraceVerdict(t, root, runID, apiv1.VerdictFail, workflow.TargetAbort, apiv1.Verdict{
		Decision:  apiv1.VerdictFail,
		Rationale: "must remain discoverable",
	}, false, "")
	if err := os.Remove(filepath.Join(root, "runs", runID, filepath.FromSlash(ref.Path))); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "trace", "--verdicts", runID, root)
	if code != 0 {
		t.Fatalf("trace --verdicts: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "gate=review decision=fail") ||
		!strings.Contains(stdout, "artifact: unavailable") {
		t.Fatalf("missing artifact was not surfaced:\n%s", stdout)
	}
}

func TestEscalationsShowIncludesVerdict(t *testing.T) {
	root := t.TempDir()
	createEscalationInspectionRun(t, root, "escalated-verdict")

	code, stdout, stderr := runArgs(t, "escalations", "show", "--include-verdict", "escalated-verdict", root)
	if code != 0 {
		t.Fatalf("escalations show --include-verdict: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "verdicts:") ||
		!strings.Contains(stdout, "gate=review decision=needs-changes target=@escalate") {
		t.Fatalf("escalation verdict missing:\n%s", stdout)
	}

	code, stdout, stderr = runArgs(t, "escalations", "show", "--json", "--include-verdict", "escalated-verdict", root)
	if code != 0 {
		t.Fatalf("escalations show --json --include-verdict: code = %d, stderr = %q", code, stderr)
	}
	var got escalationInspection
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode escalation JSON: %v", err)
	}
	if len(got.Verdicts) != 1 || got.Verdicts[0].Decision != string(apiv1.VerdictNeedsChanges) {
		t.Fatalf("escalation verdicts = %+v", got.Verdicts)
	}
}

func recordTraceVerdict(
	t *testing.T,
	root, runID string,
	decision apiv1.VerdictDecision,
	target string,
	verdict apiv1.Verdict,
	cached bool,
	diffDigest string,
) journal.Ref {
	t.Helper()
	run := newTraceTestRun(t, root, runID)
	data, err := json.Marshal(verdict)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := run.RecordArtifact("verdict/review-1.json", data)
	if err != nil {
		t.Fatal(err)
	}
	runner := map[string]any{"verdictCacheHit": cached}
	if diffDigest != "" {
		runner["diffDigest"] = diffDigest
	}
	if err := run.Append(journal.Event{
		Type:    journal.EventGateEvaluated,
		Gate:    "review",
		Verdict: string(decision),
		Target:  target,
		Name:    "verdict/review-1.json",
		Ref:     &ref,
		Runner:  runner,
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	return ref
}
