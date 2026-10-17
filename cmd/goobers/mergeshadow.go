package main

import (
	"io"
	"os"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/prstatus"
	"github.com/goobers/goobers/providers"
)

// recordMergeVerdictShadow observes the locked poll and final landing decision.
// It never writes a native status or changes a merge result. Instance annotations
// use the daemon's existing retention window (90 days by default), through
// compactSchedulerRetention -> compactInstanceJournal; pod stages use the same
// journal plane as other runner annotations.
func recordMergeVerdictShadow(root string, repo providers.RepositoryRef, pull, expectedHead string,
	poll providers.PullRequestPollResult, observation prstatus.VerdictObservation, landingAttempted bool, stdout, stderr io.Writer,
) {
	if observation == "" { // The merge lock failed before a provider poll.
		return
	}
	wouldBlock := observation.WouldBlockMissingVerdict(landingAttempted)
	if wouldBlock {
		pf(stdout, "shadow: missing merge-review verdict would block pr #%s at %s; current merge policy is unchanged\n", pull, poll.HeadSHA)
	}
	annotator, err := openStageAnnotator(layoutFor(root))
	if err != nil {
		pf(stderr, "warning: record merge verdict shadow: %v\n", err)
		return
	}
	defer func() { _ = annotator.Close() }()
	err = annotator.Append(journal.Event{
		Type: journal.EventRunnerAnnotation, Reason: "merge-verdict-shadow",
		RunID: os.Getenv(executor.RunIDEnvVar), Workflow: os.Getenv(executor.WorkflowEnvVar),
		Runner: map[string]any{
			"mode": "shadow", "provider": string(repo.Provider), "repository": repo.Owner + "/" + repo.Name,
			"pullNumber": pull, "headSHA": poll.HeadSHA, "baseSHA": poll.BaseSHA, "expectedHeadSHA": expectedHead,
			"observation": string(observation), "landingAttempted": landingAttempted, "wouldBlock": wouldBlock,
		},
	})
	if err != nil {
		pf(stderr, "warning: record merge verdict shadow: %v\n", err)
	}
}
