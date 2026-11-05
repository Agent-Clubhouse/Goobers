package childresult

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func fixture() (TerminalReturn, triggerqueue.ChildRecord) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	child := triggerqueue.ChildRecord{Identity: triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "team", ParentRunID: "parent"}, StageOccurrence: "occurrence", InvocationKey: "child-1"}, AcceptanceID: "acceptance", RunID: "child-run", ProposalDigest: journal.Digest([]byte("source")), AcceptedAt: at}
	return TerminalReturn{Version: TerminalReturnVersion, Identity: child.Identity, AcceptanceID: child.AcceptanceID, RunID: child.RunID, SourceDigest: child.ProposalDigest, Input: TerminalResultInput{State: triggerqueue.ChildCompleted, FinishedAt: at.Add(time.Minute), Summary: "Done", References: []string{"artifact:report"}}, ResultRef: journal.Digest([]byte("receipt"))}, child
}

func TestTerminalInputBounds(t *testing.T) {
	r, child := fixture()
	for _, tc := range []struct {
		name   string
		mutate func(*TerminalResultInput)
		valid  bool
	}{
		{"summary boundary", func(in *TerminalResultInput) { in.Summary = strings.Repeat("x", MaxSummaryBytes) }, true},
		{"summary bytes not runes", func(in *TerminalResultInput) { in.Summary = strings.Repeat("é", MaxSummaryBytes/2+1) }, false},
		{"invalid utf8", func(in *TerminalResultInput) { in.Summary = string([]byte{0xff}) }, false},
		{"nul summary", func(in *TerminalResultInput) { in.Summary = "a\x00b" }, false},
		{"pending", func(in *TerminalResultInput) { in.State = triggerqueue.ChildRunning }, false},
		{"predates acceptance", func(in *TerminalResultInput) { in.FinishedAt = child.AcceptedAt.Add(-time.Second) }, false},
		{"no time", func(in *TerminalResultInput) { in.FinishedAt = time.Time{} }, false},
		{"too many refs", func(in *TerminalResultInput) { in.References = make([]string, MaxReferences+1) }, false},
		{"oversize ref", func(in *TerminalResultInput) { in.References = []string{strings.Repeat("x", MaxReferenceBytes+1)} }, false},
		{"ref control", func(in *TerminalResultInput) { in.References = []string{"artifact:\nreport"} }, false},
		{"ref whitespace", func(in *TerminalResultInput) { in.References = []string{" report"} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := r.Input
			tc.mutate(&in)
			err := in.Validate(child.AcceptedAt)
			if (err == nil) != tc.valid {
				t.Fatalf("Validate=%v valid=%v", err, tc.valid)
			}
		})
	}
}

func TestTerminalReturnBindsExecutionAndEncodedBudget(t *testing.T) {
	r, child := fixture()
	if err := r.Validate(child); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*TerminalReturn)
	}{
		{"version", func(r *TerminalReturn) { r.Version++ }},
		{"tenant", func(r *TerminalReturn) { r.Identity.Gaggle = "other" }},
		{"occurrence", func(r *TerminalReturn) { r.Identity.StageOccurrence = "other" }},
		{"execution epoch", func(r *TerminalReturn) { r.RunID = "old-execution" }},
		{"acceptance", func(r *TerminalReturn) { r.AcceptanceID = "other" }},
		{"source", func(r *TerminalReturn) { r.SourceDigest = journal.Digest([]byte("other")) }},
		{"receipt", func(r *TerminalReturn) { r.ResultRef = "not-a-digest" }},
		{"workspace", func(r *TerminalReturn) { r.WorkspaceRef = "invalid\nref" }},
		{"json escaping exceeds budget", func(r *TerminalReturn) { r.Input.Summary = strings.Repeat("<", MaxSummaryBytes) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := r
			tc.mutate(&copy)
			if !errors.Is(copy.Validate(child), ErrInvalidTerminalReturn) {
				t.Fatal("invalid return accepted")
			}
		})
	}
	// The input summary alone is legal; the encoded transport has a separate cap.
	r.Input.Summary = strings.Repeat("<", MaxSummaryBytes)
	if err := r.Input.Validate(child.AcceptedAt); err != nil {
		t.Fatal(err)
	}
}
