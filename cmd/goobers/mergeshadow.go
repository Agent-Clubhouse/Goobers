package main

import (
	"io"
	"os"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/prstatus"
	"github.com/goobers/goobers/providers"
)

// recordMergeVerdictShadow emits one best-effort observation per locked poll,
// after the existing landing decision. The journal's configured daemon age
// compaction applies while its sweep runs; this is not a byte cap or deduplication.
func recordMergeVerdictShadow(root string, repo providers.RepositoryRef, pull, expectedHead, expectedBase string,
	poll providers.PullRequestPollResult, observation prstatus.VerdictObservation, landingAttempted bool, outcome string, stdout, stderr io.Writer,
) {
	if observation.Presence == "" { // The merge lock failed before a provider poll.
		return
	}
	wouldBlock := observation.WouldBlockMissingVerdict(landingAttempted)
	if wouldBlock {
		pf(stdout, "shadow: missing merge-review verdict would block pr #%s at %s; current merge policy is unchanged\n", pull, poll.HeadSHA)
	}
	annotator, err := openStageAnnotator(layoutFor(root))
	if err != nil {
		pf(stderr, "warning: merge verdict shadow annotation unavailable (open)\n")
		return
	}
	defer func() { _ = annotator.Close() }()
	err = annotator.Append(journal.Event{
		Type: journal.EventRunnerAnnotation, Reason: "merge-verdict-shadow",
		RunID: os.Getenv(executor.RunIDEnvVar), Workflow: os.Getenv(executor.WorkflowEnvVar),
		Runner: map[string]any{
			"mode": "shadow", "provider": string(repo.Provider), "repository": repo.CanonicalKey(),
			"pullNumber": pull, "headSHA": poll.HeadSHA, "baseSHA": poll.BaseSHA,
			"expectedHeadSHA": expectedHead, "expectedBaseSHA": expectedBase,
			"observation": observation.Presence, "reasonClass": observation.Reason, "pollPhase": observation.Phase,
			"verdictState": observation.State, "baseMatch": observation.BaseMatch, "outcome": outcome,
			"landingAttempted": landingAttempted, "wouldBlock": wouldBlock,
		},
	})
	if err != nil {
		pf(stderr, "warning: merge verdict shadow annotation unavailable (append)\n")
	}
}

// mergeShadowOutcome names only the branch the existing merge path took. Error
// values, label text, status descriptions and refusal strings are never exported.
func mergeShadowOutcome(lockErr, pollErr error, optOut string, advisory bool, reasons []string, commitErr, checkErr, policyErr error, attempted bool, mergeErr error) string {
	switch {
	case lockErr != nil:
		return "lock-error"
	case pollErr != nil:
		return "poll-error"
	case optOut == runAbortedOptOutReason:
		return "run-aborted"
	case optOut != "":
		return "opted-out"
	case advisory:
		return "advisory"
	case len(reasons) > 0:
		return "existing-gate-refusal"
	case commitErr != nil:
		return "commit-message-error"
	case checkErr != nil:
		return "premerge-check-error"
	case policyErr != nil:
		return "policy-error"
	case attempted && mergeErr != nil:
		return "landing-error"
	case attempted:
		return "landing-submitted"
	default:
		return "not-attempted"
	}
}
