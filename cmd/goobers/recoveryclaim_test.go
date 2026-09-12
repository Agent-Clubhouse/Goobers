package main

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestRecoveryClaimMatchingRequiresUnambiguousCompleteIdentity(t *testing.T) {
	const runID, repoKey = "retained-run", "gitea|forge-one.example||team|repo|"
	annotation := func(run, repo, issue, kind string) journal.Event {
		return journal.Event{Type: journal.EventRunnerAnnotation, RunID: run, Runner: map[string]any{
			"annotation": itemRepoAnnotation, "key": itemRepoKey(run, issue), "repositoryKey": repo, "itemId": issue, "kind": kind,
		}}
	}
	for _, mode := range []string{"match", "retry", "foreign-run", "foreign-forge", "pull-request", "other-issue", "legacy", "key-mismatch", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			event := annotation(runID, repoKey, "7", itemKindIssue)
			events := []journal.Event{event}
			wantErr := false
			switch mode {
			case "retry":
				events = append(events, annotation(runID, repoKey, "7", itemKindIssue))
			case "foreign-run":
				events[0] = annotation("another-run", repoKey, "7", itemKindIssue)
			case "foreign-forge":
				event.Runner["repositoryKey"] = "gitea|forge-two.example||team|repo|"
			case "pull-request":
				event.Runner["kind"] = itemKindPullRequest
			case "other-issue":
				events[0] = annotation(runID, repoKey, "8", itemKindIssue)
			case "legacy":
				delete(event.Runner, "repositoryKey")
				wantErr = true
			case "key-mismatch":
				event.Runner["key"] = itemRepoKey("another-run", "7")
				wantErr = true
			case "conflict":
				events = append(events, annotation(runID, repoKey, "8", itemKindIssue))
				wantErr = true
			}
			matched, err := recoveryClaimMatches(events, runID, repoKey, "7")
			if want := mode == "match" || mode == "retry"; matched != want || (err != nil) != wantErr {
				t.Fatalf("matched=%t err=%v, want match=%t error=%t", matched, err, want, wantErr)
			}
		})
	}
}
