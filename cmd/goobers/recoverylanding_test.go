package main

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
)

func TestRecoveryLandingHeadsUsesTerminalOwningJournal(t *testing.T) {
	for _, mode := range []string{"paired", "recovered", "busy", "active", "foreign-repository", "failed-receipt", "intent-only", "split-journals"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "app"}
			key := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name}.CanonicalKey()
			route, err := recoveryLandingRoute(repo)
			if err != nil {
				t.Fatal(err)
			}
			intent := providers.LandingIntent{ID: "intent", Operation: "merge", RepositoryAPIURL: route, PullID: "42", ExpectedHeadSHA: strings.Repeat("a", 40)}
			confirmation := providers.MergeConfirmation{IntentID: intent.ID, RepositoryAPIURL: route, PullID: "42", MergeSHA: strings.Repeat("b", 40)}
			if mode == "foreign-repository" {
				repo.Name = "other"
			}
			create := func(id string, includeIntent, includeReceipt bool) {
				t.Helper()
				run, err := journal.Create(root, journal.RunIdentity{RunID: id, Workflow: "restore", WorkflowVersion: 1, WorkspaceRepository: &repo}, nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = run.Close() })
				appendEvent := func(fields map[string]any, receipt bool) {
					t.Helper()
					event := journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "42"}, Runner: fields}
					if mode == "recovered" {
						event.Type = journal.EventRunnerMutationRecovered
					}
					if mode == "failed-receipt" && receipt {
						event.Type = journal.EventError
						event.Error = &journal.ErrorDetail{Code: "failed", Message: "not confirmed"}
					}
					if err := run.Append(event); err != nil {
						t.Fatal(err)
					}
				}
				if includeIntent {
					appendEvent(providers.MutationRunnerFields("merge-intent", nil, nil, &intent), false)
				}
				if includeReceipt {
					appendEvent(providers.MutationRunnerFields("merge", &confirmation, nil, nil), true)
				}
				if mode != "active" {
					if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
						t.Fatal(err)
					}
				}
				if mode != "busy" {
					if err := run.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			create("receiving-run", true, mode != "intent-only" && mode != "split-journals")
			if mode == "split-journals" {
				create("different-run", false, true)
			}
			heads, err := recoveryLandingHeads(t.Context(), root, recovery.Record{RunID: "source-run", RepositoryKey: key}, route)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "paired" || mode == "recovered" {
				want = 1
			}
			if len(heads) != want {
				t.Fatalf("heads=%+v, want %d", heads, want)
			}
		})
	}
}
