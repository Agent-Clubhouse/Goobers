package gate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestEvaluatorDisprovesTransportEscapedRawStringFinding(t *testing.T) {
	run := newTestJournal(t)
	pointer := reviewerDiffPointer(t, run, strings.Join([]string{
		"diff --git a/internal/procenv/procenv_test.go b/internal/procenv/procenv_test.go",
		"--- a/internal/procenv/procenv_test.go",
		"+++ b/internal/procenv/procenv_test.go",
		"@@ -0,0 +1 @@",
		"+const packageJSON = `{\"name\":\"example\",\"version\":\"1.0.0\"}`",
		"",
	}, "\n"))
	env := transportedEnvelope(t, apiv1.InvocationEnvelope{
		ContextPointers: []apiv1.ContextPointer{pointer},
	})
	reviewer := &fakeGoober{reviewVerdict: apiv1.Verdict{
		Decision:  apiv1.VerdictNeedsChanges,
		Rationale: "the transport shows escaped quotes",
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError,
			Message:  "the raw-string fixture is invalid JSON because its quotes are escaped",
			Location: "internal/procenv/procenv_test.go:1",
		}},
	}}
	evaluator := &Evaluator{
		Reviewer: &ReviewerEvaluator{Goober: reviewer},
		Journal:  run,
	}

	result, err := evaluator.Evaluate(
		context.Background(),
		reviewerEvidenceGate(),
		env,
		"implement",
		apiv1.ResultEnvelope{},
		"sha256:first",
		false,
	)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Outcome != string(apiv1.VerdictPass) || result.Target != wf.TerminalComplete {
		t.Fatalf("result = %+v, want pass to complete", result)
	}
	if result.Reason != ReasonFindingDisproven || result.Attempt != 0 || result.Escalated {
		t.Fatalf("result = %+v, want disproven reason without consuming a repass", result)
	}
	if result.Verdict == nil || result.Verdict.Decision != apiv1.VerdictPass || len(result.Verdict.Findings) != 0 {
		t.Fatalf("normalized verdict = %+v, want pass with the false finding removed", result.Verdict)
	}
	if !strings.Contains(result.Verdict.Rationale, "encoding/json.Valid") {
		t.Fatalf("rationale = %q, want deterministic parse evidence", result.Verdict.Rationale)
	}

	events := readGateEvents(t, run)
	if len(events) != 1 || events[0].Runner["reason"] != ReasonFindingDisproven {
		t.Fatalf("gate events = %+v, want journaled disproval reason", events)
	}
}

func TestReviewerFindingDisprovalPreservesUnresolvedFindings(t *testing.T) {
	run := newTestJournal(t)
	pointer := reviewerDiffPointer(t, run, strings.Join([]string{
		"diff --git a/fixture.go b/fixture.go",
		"--- a/fixture.go",
		"+++ b/fixture.go",
		"@@ -0,0 +1 @@",
		"+const fixture = `{\"valid\":true}`",
		"",
	}, "\n"))
	reviewer := &fakeGoober{reviewVerdict: apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{
			{
				Severity: apiv1.SeverityError,
				Message:  "the raw-string JSON fixture fails to parse",
				Location: "fixture.go:1",
			},
			{
				Severity: apiv1.SeverityError,
				Message:  "the implementation is missing a boundary test",
				Location: "fixture_test.go:20",
			},
		},
	}}
	evaluator := &Evaluator{
		Reviewer: &ReviewerEvaluator{Goober: reviewer},
		Journal:  run,
	}

	result, err := evaluator.Evaluate(
		context.Background(),
		reviewerEvidenceGate(),
		apiv1.InvocationEnvelope{ContextPointers: []apiv1.ContextPointer{pointer}},
		"implement",
		apiv1.ResultEnvelope{},
		"sha256:first",
		false,
	)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Outcome != string(apiv1.VerdictNeedsChanges) || result.Target != "implement" || result.Attempt != 1 {
		t.Fatalf("result = %+v, want unresolved finding to consume one ordinary repass", result)
	}
	if result.Reason != "" || result.Verdict == nil || len(result.Verdict.Findings) != 1 {
		t.Fatalf("normalized partial verdict = %+v, want only the unresolved finding", result.Verdict)
	}
	if result.Verdict.Findings[0].Location != "fixture_test.go:20" {
		t.Fatalf("remaining finding = %+v, want missing-test finding", result.Verdict.Findings[0])
	}
	if !strings.Contains(result.Verdict.Rationale, ReasonFindingDisproven) {
		t.Fatalf("rationale = %q, want partial disproval evidence", result.Verdict.Rationale)
	}
}

func TestReviewerFindingDisprovalRequiresUnambiguousExactSource(t *testing.T) {
	tests := []struct {
		name     string
		location string
		source   string
	}{
		{
			name:     "two raw strings on claimed line",
			location: "fixture.go:1",
			source:   "const invalid = `not json`; const unrelated = `{}`",
		},
		{
			name:     "actual invalid raw string",
			location: "fixture.go:1",
			source:   "const fixture = `not json`",
		},
		{
			name:     "valid raw string on another line",
			location: "fixture.go:1",
			source:   "const invalid = `not json`\n+const valid = `{}`",
		},
		{
			name:     "missing concrete line",
			location: "fixture.go",
			source:   "const fixture = `{}`",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := newTestJournal(t)
			lines := strings.Split(tc.source, "\n")
			patch := strings.Join([]string{
				"diff --git a/fixture.go b/fixture.go",
				"--- a/fixture.go",
				"+++ b/fixture.go",
				"@@ -0,0 +1," + string(rune('0'+len(lines))) + " @@",
				"+" + strings.Join(lines, "\n"),
				"",
			}, "\n")
			pointer := reviewerDiffPointer(t, run, patch)
			verdict := apiv1.Verdict{
				Decision: apiv1.VerdictNeedsChanges,
				Findings: []apiv1.Finding{{
					Severity: apiv1.SeverityError,
					Message:  "the raw-string JSON fixture fails to parse",
					Location: tc.location,
				}},
			}

			normalized, allDisproven := disproveReviewerFindings(
				verdict,
				[]apiv1.ContextPointer{pointer},
				run.Dir(),
				"review",
			)
			if allDisproven || normalized.Decision != apiv1.VerdictNeedsChanges || len(normalized.Findings) != 1 {
				t.Fatalf("normalized = %+v, allDisproven = %v; want finding preserved", normalized, allDisproven)
			}
		})
	}
}

func TestReviewerFindingDisprovalPreservesSemanticJSONFinding(t *testing.T) {
	run := newTestJournal(t)
	pointer := reviewerDiffPointer(t, run, strings.Join([]string{
		"diff --git a/fixture.go b/fixture.go",
		"--- a/fixture.go",
		"+++ b/fixture.go",
		"@@ -0,0 +1 @@",
		"+const fixture = `{\"enabled\":\"true\"}`",
		"",
	}, "\n"))
	verdict := apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError,
			Message:  "invalid JSON for the schema: enabled must be a boolean, not a string",
			Location: "fixture.go:1",
		}},
	}

	normalized, allDisproven := disproveReviewerFindings(
		verdict,
		[]apiv1.ContextPointer{pointer},
		run.Dir(),
		"review",
	)
	if allDisproven || normalized.Decision != apiv1.VerdictNeedsChanges || len(normalized.Findings) != 1 {
		t.Fatalf("normalized = %+v, allDisproven = %v; want semantic finding preserved", normalized, allDisproven)
	}
}

func TestReviewerFindingDisprovalIgnoresNonAuthoritativeDiff(t *testing.T) {
	run := newTestJournal(t)
	authoritative := reviewerDiffPointer(t, run, strings.Join([]string{
		"diff --git a/fixture.go b/fixture.go",
		"--- a/fixture.go",
		"+++ b/fixture.go",
		"@@ -0,0 +1 @@",
		"+const fixture = `not json`",
		"",
	}, "\n"))
	other := reviewerDiffPointer(t, run, strings.Join([]string{
		"diff --git a/fixture.go b/fixture.go",
		"--- a/fixture.go",
		"+++ b/fixture.go",
		"@@ -0,0 +1 @@",
		"+const fixture = `{}`",
		"",
	}, "\n"))
	other.Name = "implement.patch"
	verdict := apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError,
			Message:  "the raw-string JSON fixture fails to parse",
			Location: "fixture.go:1",
		}},
	}

	normalized, allDisproven := disproveReviewerFindings(
		verdict,
		[]apiv1.ContextPointer{authoritative, other},
		run.Dir(),
		"review",
	)
	if allDisproven || normalized.Decision != apiv1.VerdictNeedsChanges || len(normalized.Findings) != 1 {
		t.Fatalf("normalized = %+v, allDisproven = %v; want authoritative invalid source to preserve finding", normalized, allDisproven)
	}
}

func reviewerEvidenceGate() apiv1.Gate {
	return apiv1.Gate{
		Name:      "review",
		Evaluator: apiv1.EvaluatorAgentic,
		Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
		Branches: map[string]string{
			string(apiv1.VerdictPass):         wf.TerminalComplete,
			string(apiv1.VerdictNeedsChanges): "implement",
			string(apiv1.VerdictFail):         wf.TargetAbort,
		},
	}
}

func reviewerDiffPointer(t *testing.T, run Journal, patch string) apiv1.ContextPointer {
	t.Helper()
	ref, err := run.RecordArtifact("reviewer-diff.patch", []byte(patch))
	if err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	return apiv1.ContextPointer{
		Name: "review.diff",
		Artifact: &apiv1.ArtifactPointer{
			Path:      ref.Path,
			Digest:    ref.Digest,
			Size:      ref.Size,
			MediaType: "text/x-diff",
			Integrity: ref.Integrity,
		},
	}
}

func transportedEnvelope(t *testing.T, env apiv1.InvocationEnvelope) apiv1.InvocationEnvelope {
	t.Helper()
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var transported apiv1.InvocationEnvelope
	if err := json.Unmarshal(data, &transported); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	return transported
}
