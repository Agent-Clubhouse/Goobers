// Package terminalhooks is the runner-free home of the terminal-hook frame: the
// instance-level side effects a run's terminal is supposed to have (existing-fix
// handling, blocked parking and escalation, failure traces, branch cleanup,
// notifications, claim release, attribution), fired in the local runner's order
// from one finished run.
//
// The outcome and handler types live here, not in internal/runner, so the cloud
// controller can fire the same frame without importing the local runner;
// internal/runner aliases them back so local call sites are unchanged.
package terminalhooks

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry"
)

// BlockedOutcome describes a run terminating because a stage reported status
// "blocked" (#544/#545) — the value Config.Blocked receives.
type BlockedOutcome struct {
	RunID string
	// RepoRef is the target repository containing the driving backlog item.
	RepoRef apiv1.RepoRef
	// Stage is the stage that reported blocked.
	Stage string
	// ItemID is the driving backlog item's id when the run was started with
	// one (StartInput.Item). Empty for a run that claims its item mid-run
	// (schedule/backlog-item-triggered implementation runs) — the handler
	// resolves those from the claim ledger by RunID instead.
	ItemID string
	// Reason is the agent's stated reason for the block (its error detail,
	// falling back to its summary).
	Reason string
	// Blockers are the blocking issue numbers the stage referenced via the
	// documented outputs.blockedBy convention (comma-separated numbers in a
	// scalar string — see docs/stage-contract.md). Empty when the stage named
	// none in machine-readable form. Any reference to the driving item itself
	// is dropped before the handler sees it (#2961); handlers resolving items
	// from the claim ledger must apply FilterSelfBlockers per resolved item.
	Blockers []string
}

// BlockedHandler is Config.Blocked's shape. Implementations are instance-level
// (composition-root) policy: record the block for selection to skip (#552),
// and park the driving issue. Must tolerate an empty ItemID.
type BlockedHandler func(ctx context.Context, o BlockedOutcome) error

// FailedOutcome describes a run terminating at PhaseFailed (#1054) — the value
// Config.Failed receives. Distinct from the escalated/blocked terminals (which
// park the driving issue goobers:needs-human): a `failed` terminal must leave a
// human-visible trace WITHOUT that label, so repeated failures on the same item
// — a recurring copilot-cli harness session timeout is the motivating case —
// accumulate a countable signal instead of the item silently returning to
// goobers:ready with no record.
type FailedOutcome struct {
	RunID string
	// Seq identifies the journal event whose terminal consequence is surfaced.
	Seq uint64
	// RepoRef is the target repository containing the driving backlog item —
	// the handler resolves its per-repo credential and posts the trace comment
	// to this repo, mirroring BlockedOutcome.
	RepoRef apiv1.RepoRef
	// Stage is the failing stage/gate name when the failure is attributable to
	// one (a stage-reported business failure, a gate-eval error). Empty for a
	// state-less walk-level failure (max-steps, an unknown state) — the
	// harness-timeout case (a dispatch-level runTask error) carries the stage
	// that was executing.
	Stage string
	// Cause is the run's terminal error message, journaled in the run_failed
	// cause event for local diagnostics. Handlers must not publish this value to
	// external work items because nested execution errors can contain sensitive
	// prompts, argv, credentials, environment values, or context.
	Cause string
	// Code is the stable machine-readable failure code the run terminated
	// with — the same value Result.FailureCode carries (a stage's own typed
	// code where one exists, else "run_failed"). Handlers use it to classify
	// the terminal's disposition (telemetry.ClassifyError): an infra-fault
	// terminal (#3361) must not feed work-quality signals like the
	// failure-streak circuit breaker (#3364).
	Code string
	// FaultClass is the runner's own classification of the terminal, set
	// only where the runner KNOWS who owns the fault rather than having to
	// re-derive it from Code (#5638): today, telemetry.ErrorClassInfra for a
	// dispatch that exhausted its infrastructure retry budget — the same
	// failure the runner already journaled as retryFailureClass "infra".
	// Empty means "not classified here": consumers fall back to
	// telemetry.ClassifyError(Code). A session timeout is never classified
	// infra here (invoke.Timeout is not an infrastructure marker), so the
	// #1054 timeout case still reaches consumers by its code.
	FaultClass telemetry.ErrorClass
}

// FailedHandler is Config.Failed's shape. Implementations are instance-level
// (composition-root) policy: leave a human-visible trace on the driving item
// when a run ends PhaseFailed (#1054). Must tolerate a run with no driving
// item.
type FailedHandler func(ctx context.Context, o FailedOutcome) error

// ExistingFixOutcome describes a run terminating with no-work from the
// implement stage because the fix for the claimed issue already exists on
// main (issue #3236) — the value Config.ExistingFix receives.
type ExistingFixOutcome struct {
	RunID string
	// ItemID is the backlog item id whose fix already exists.
	ItemID string
	// RepoRef is the target repository.
	RepoRef apiv1.RepoRef
	// Commit is the SHA of the commit that fixes the issue.
	Commit string
}

// ExistingFixHandler is Config.ExistingFix's shape. Implementations are
// instance-level (composition-root) policy: strip goobers:ready and
// goobers:critical labels from the item to prevent reclaim, and optionally
// close the issue, when a no-work completion indicates the fix already exists
// on main (issue #3236).
type ExistingFixHandler func(ctx context.Context, o ExistingFixOutcome) error
