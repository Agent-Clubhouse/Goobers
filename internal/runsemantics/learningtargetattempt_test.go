package runsemantics

import (
	"encoding/json"
	"testing"

	// Coverage for #3931: a learning episode's nextAttempt — and the attempt its
	// annotation is journaled under — belong to the stage being RE-ENTERED, not to
	// the stage that failed.
	//
	// Every learning-episode fixture in the tree before this one walked
	// `implement -> review -> implement`, the TRIVIAL send-back, where the subject
	// and the target are the same stage and subjectAttempt+1 happens to equal the
	// target's next attempt. That is why nothing caught the defect: the two
	// derivations are indistinguishable on the only shape anyone had written down.
	//
	// The fixtures here walk a NONTRIVIAL send-back — a gate whose fail branch
	// re-enters a stage other than its subject, which is the shape every shipped
	// lane actually uses (implementation.yaml's `local-gate: fail -> implement`
	// over a `local-ci` subject, `ci-gate: fail -> remediate-ci` over `ci-poll`,
	// pr-remediation's `finding-responses-gate: fail -> guard-before-implement`
	// over `validate-finding-responses`) — and one where the target's attempt count
	// has already advanced past the subject's, which is what makes the two numbers
	// observably different.
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// The unit statement of the derivation, over a hand-built event log: the
// target's next attempt counts the target's own ENTRIES, scoped to the branch
// the failure happened in, and it ignores both the policy retries inside one
// entry and the annotations an injection itself writes.
func TestResolveLearningEpisodeAddressingReadsTheTargetsOwnHistory(t *testing.T) {
	events := []journal.Event{
		{Seq: 1, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		// A Task.Retry policy attempt inside the SAME entry. journal.Attempt
		// is per-entry, so this must not count as a re-entry.
		{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 2,
			AttemptClass: journal.AttemptPolicy},
		{Seq: 3, Type: journal.EventStageFinished, Stage: "implement", Attempt: 2, Status: string(apiv1.ResultSuccess)},
		// The first repass's own annotation: attributed to implement at the
		// attempt it FEEDS. Counting it would make each injection advance the
		// number the next one reads.
		{Seq: 4, Type: journal.EventRunnerAnnotation, Stage: "implement", Attempt: 2,
			Runner: map[string]any{"kind": LearningEpisodeInjectedKind}},
		// The second ENTRY: a repass restarts the per-entry counter at 1.
		{Seq: 5, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Seq: 6, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Seq: 7, Type: journal.EventStageStarted, Stage: "local-ci", Attempt: 1},
		{Seq: 8, Type: journal.EventStageFinished, Stage: "local-ci", Attempt: 1, Status: string(apiv1.ResultFailure)},
		// A sibling branch walking a stage of the same name, further along.
		// Counting it would address the correction to somebody else's entry.
		{Seq: 9, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1, Branch: 2},
		{Seq: 10, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1, Branch: 2},
	}

	got := ResolveLearningEpisodeAddressing(events, "local-gate", "local-ci", "implement", false)
	want := LearningEpisodeAddressing{SourceSeq: 8, SourceAttempt: 1, TargetNextAttempt: 3}
	if got != want {
		t.Fatalf("addressing = %+v, want %+v — the subject (local-ci) is on its first entry while the "+
			"target (implement) has already been entered twice, so the correction is addressed to "+
			"implement's third", got, want)
	}

	// The reviewer arm resolves the same target arithmetic off gate.evaluated.
	reviewerEvents := append(events[:8:8], journal.Event{
		Seq: 9, Type: journal.EventGateEvaluated, Gate: "review", Target: "implement",
		Runner: map[string]any{"repassAttempt": 2},
	})
	gotReviewer := ResolveLearningEpisodeAddressing(reviewerEvents, "review", "local-ci", "implement", true)
	wantReviewer := LearningEpisodeAddressing{SourceSeq: 9, SourceAttempt: 2, TargetNextAttempt: 3}
	if gotReviewer != wantReviewer {
		t.Fatalf("reviewer addressing = %+v, want %+v", gotReviewer, wantReviewer)
	}

	// A target with no history at all reports 0 — the forward-branch shape
	// LearningEpisodeAppliesToRepass refuses, and the builder's fallback.
	forward := ResolveLearningEpisodeAddressing(events, "local-gate", "local-ci", "park-failed", false)
	if forward.TargetNextAttempt != 0 {
		t.Fatalf("forward-branch targetNextAttempt = %d, want 0 — park-failed has never run",
			forward.TargetNextAttempt)
	}
}

// The compatibility statement, executable: with no target history to read, the
// builder reproduces the PRE-#3931 derivation byte for byte.
//
// This is what the engine's Temporal version guard depends on. A worker
// replaying a history recorded before the change gets DefaultVersion, passes
// TargetNextAttempt 0, and must re-derive the identical episode — same
// nextAttempt, same bytes, same digest. If this fallback ever changed, every
// pre-change history would start replaying to a different artifact than the
// one it recorded.
//
// The second half bounds the migration. learning.EpisodeID is content-addressed
// over runId + sourceSeq + finding identities ONLY, so the join key every
// cross-run learning consumer correlates on — gate.readEpisodeHistory,
// reconciliation, the signature continuity the repass loop depends on — is
// NOT moved by this change. What moves is the episode's serialized bytes and
// therefore the artifact digest, which is why the engine's switch is versioned
// and the runner's is not: the runner re-reads recorded artifacts, the engine
// re-derives them on replay.
func TestBuildLearningEpisodeFallsBackToThePreChangeDerivation(t *testing.T) {
	in := learningEpisodeTestInput()
	in.SourceAttempt = 4
	in.TargetNextAttempt = 0
	legacy := BuildLearningEpisode(in)
	if legacy.NextAttempt != legacy.SourceAttempt+1 {
		t.Fatalf("fallback nextAttempt = %d, want sourceAttempt+1 (%d) — this is the derivation every "+
			"pre-#3931 history replays against", legacy.NextAttempt, legacy.SourceAttempt+1)
	}

	in.TargetNextAttempt = 7
	migrated := BuildLearningEpisode(in)
	if migrated.NextAttempt != 7 {
		t.Fatalf("nextAttempt = %d, want the target's own next attempt (7)", migrated.NextAttempt)
	}
	if migrated.SourceAttempt != 4 {
		t.Fatalf("sourceAttempt = %d, want the SUBJECT's attempt (4) — it is what makes the episode say "+
			"which failure it is about, and #3931 does not move it", migrated.SourceAttempt)
	}
	legacyData, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("encode legacy episode: %v", err)
	}
	migratedData, err := json.Marshal(migrated)
	if err != nil {
		t.Fatalf("encode migrated episode: %v", err)
	}
	if string(legacyData) == string(migratedData) {
		t.Fatal("the two derivations produced identical bytes; nextAttempt is serialized into the " +
			"episode, so the artifact digest must move — that is the migration being budgeted")
	}
	if migrated.ID != legacy.ID {
		t.Fatalf("episode id moved from %q to %q; EpisodeID is addressed over runId + sourceSeq + "+
			"finding identities, NOT over the attempt fields, and the whole cross-run correlation "+
			"surface joins on it. If this ever starts moving, the migration is a different and much "+
			"larger one than #3931 budgeted for", legacy.ID, migrated.ID)
	}
}
