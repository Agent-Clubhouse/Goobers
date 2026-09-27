package main

import (
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
)

// seedOpenPRMetadataJournal writes the run journal open-pr reads its fallback
// metadata out of. claimIssue records the query-backlog claim (the source of the
// claimed title and the `Fixes #N` linkage); reviewEvidence additionally records
// the review verdict + local-ci result that make renderStructuredPRBody produce a
// structured body rather than the one-line generic one. Keeping the two
// independent is the point: it is what lets a case below assert that an explicit
// body input bypasses structured rendering that WOULD otherwise have happened,
// as opposed to merely filling a gap where no evidence existed.
func seedOpenPRMetadataJournal(t *testing.T, root, runID string, claimIssue, reviewEvidence bool) {
	t.Helper()
	if !claimIssue && !reviewEvidence {
		return
	}
	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowDigest: journal.Digest([]byte("workflow")),
		Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	if claimIssue {
		if err := run.Append(journal.Event{
			Type: journal.EventStageFinished, Stage: "query-backlog", Attempt: 1, Status: "success",
			Outputs: map[string]any{
				"id": "42", "title": "Claimed issue title", "body": "## Problem\nSomething.",
				"updatedAt": "2026-09-01T12:00:00Z",
			},
		}); err != nil {
			t.Fatalf("record claimed issue: %v", err)
		}
	}
	if reviewEvidence {
		verdict, err := json.Marshal(apiv1.Verdict{
			Decision: apiv1.VerdictPass,
			Summary:  "Structured summary line.",
		})
		if err != nil {
			t.Fatalf("marshal verdict: %v", err)
		}
		verdictRef, err := run.RecordArtifact("verdict/review-1.json", verdict)
		if err != nil {
			t.Fatalf("record verdict: %v", err)
		}
		if err := run.Append(journal.Event{
			Type: journal.EventGateEvaluated, Gate: "review", Verdict: string(apiv1.VerdictPass),
			Target: "local-ci", Name: "verdict/review-1.json", Ref: &verdictRef,
			Runner: map[string]any{"repassAttempt": 1},
		}); err != nil {
			t.Fatalf("record review event: %v", err)
		}
		stdoutRef, err := run.RecordArtifact(runID+":local-ci/stdout.log", []byte(
			"ok  \tgithub.com/goobers/goobers/cmd/goobers\t1.000s\n",
		))
		if err != nil {
			t.Fatalf("record local-ci stdout: %v", err)
		}
		if err := run.Append(journal.Event{
			Type: journal.EventStageFinished, Stage: "local-ci", Attempt: 1, Status: "success",
			Artifacts: []journal.Ref{stdoutRef},
		}); err != nil {
			t.Fatalf("record local-ci result: %v", err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}

// TestOpenPRMetadataPrecedence is #5263's acceptance: the title/body contract
// open-pr actually implements, pinned case by case so the documented precedence
// in openPRHelp cannot drift away from the code silently.
//
// #5263 is a discoverability defect, not the historical always-generic-title bug
// (#241): claimed titles and structured bodies already work. What was missing was
// a statement of which input wins, and coverage proving an EMPTY input is a
// fallback rather than an override — providerInput treats "" as absent, so a
// workflow that threads an unset value through inputsFrom gets the fallback, not
// an empty PR title.
func TestOpenPRMetadataPrecedence(t *testing.T) {
	const genericTitle = "Automated implementation"
	const genericBody = "Automated PR opened by the goobers implementation workflow."

	for _, tc := range []struct {
		name           string
		claimIssue     bool
		reviewEvidence bool
		inputs         map[string]string
		wantTitle      string
		wantBody       []string
		wantNotBody    []string
	}{
		{
			// No claim and no inputs: both fallbacks bottom out in the generic
			// values. No issue was claimed, so there is no linkage to append.
			name:        "generic when nothing claimed and nothing set",
			wantTitle:   genericTitle,
			wantBody:    []string{genericBody},
			wantNotBody: []string{"Fixes #"},
		},
		{
			// The claim supplies the title; with no review/local-ci evidence the
			// body stays generic but still earns the `Fixes #N` back-reference.
			name:        "claimed title and linkage on a generic body",
			claimIssue:  true,
			wantTitle:   "Claimed issue title",
			wantBody:    []string{genericBody, "Fixes #42"},
			wantNotBody: []string{"## Summary"},
		},
		{
			name:       "explicit title wins over the claimed title",
			claimIssue: true,
			inputs:     map[string]string{"title": "Explicit title"},
			wantTitle:  "Explicit title",
			wantBody:   []string{"Fixes #42"},
		},
		{
			// The empty-input case. An explicitly declared but empty title must
			// NOT win as an empty PR title.
			name:       "empty title input falls back to the claimed title",
			claimIssue: true,
			inputs:     map[string]string{"title": ""},
			wantTitle:  "Claimed issue title",
		},
		{
			name:      "empty title input with no claim falls back to generic",
			inputs:    map[string]string{"title": ""},
			wantTitle: genericTitle,
		},
		{
			// Recorded evidence present, so renderStructuredPRBody would have
			// produced a structured body — the explicit input bypasses it, and
			// the claim still augments the resulting unstructured body.
			name:           "explicit body bypasses structured rendering but keeps linkage",
			claimIssue:     true,
			reviewEvidence: true,
			inputs:         map[string]string{"body": "Explicit body text."},
			wantTitle:      "Claimed issue title",
			wantBody:       []string{"Explicit body text.", "Fixes #42"},
			wantNotBody:    []string{"## Summary", "Structured summary line.", "## Testing"},
		},
		{
			// Same evidence, no explicit body: the structured body renders and is
			// NOT appended to, because it carries its own linkage already.
			name:           "structured body renders and is not appended to",
			claimIssue:     true,
			reviewEvidence: true,
			wantTitle:      "Claimed issue title",
			wantBody:       []string{"## Summary", "Implements #42", "Structured summary line."},
			wantNotBody:    []string{genericBody, "\n\nFixes #42"},
		},
		{
			name:           "empty body input falls back to the structured body",
			claimIssue:     true,
			reviewEvidence: true,
			inputs:         map[string]string{"body": ""},
			wantTitle:      "Claimed issue title",
			wantBody:       []string{"## Summary", "Structured summary line."},
			wantNotBody:    []string{genericBody},
		},
		{
			name:      "explicit title and body together, no claim",
			inputs:    map[string]string{"title": "Both set", "body": "Body set."},
			wantTitle: "Both set",
			wantBody:  []string{"Body set."},
			wantNotBody: []string{
				genericTitle, genericBody, "Fixes #",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			runID := "run-5263"
			providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.ProviderPRWrite)), runID)
			seedOpenPRMetadataJournal(t, root, runID, tc.claimIssue, tc.reviewEvidence)
			for key, value := range tc.inputs {
				t.Setenv(executor.InputEnvVar(key), value)
			}

			t.Chdir(t.TempDir())
			if code, stdout, stderr := runArgs(t, "open-pr", root); code != 0 {
				t.Fatalf("open-pr: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}

			server.mu.Lock()
			pr := server.prs[1]
			server.mu.Unlock()
			if pr == nil {
				t.Fatal("no PR opened")
			}
			if pr.title != tc.wantTitle {
				t.Errorf("PR title = %q, want %q", pr.title, tc.wantTitle)
			}
			for _, want := range tc.wantBody {
				if !strings.Contains(pr.body, want) {
					t.Errorf("PR body missing %q:\n%s", want, pr.body)
				}
			}
			for _, unwanted := range tc.wantNotBody {
				if strings.Contains(pr.body, unwanted) {
					t.Errorf("PR body unexpectedly contains %q:\n%s", unwanted, pr.body)
				}
			}
		})
	}
}
