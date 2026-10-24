package terminalawait

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/terminalhooks"
	"github.com/goobers/goobers/internal/terminalhooks/frame"
)

// Releaser releases every claim a run holds (claims.Plane.ReleaseRun).
type Releaser interface {
	ReleaseRun(ctx context.Context, handle *lease.Handle, runID string, reason claimrelease.Reason) ([]localscheduler.ClaimEntry, error)
}

// Issue references for the parts of the terminal frame the cloud cannot do yet.
const (
	// MarkerRetireIssue tracks the provider claim-marker retire, which needs
	// provider credentials in the control role.
	MarkerRetireIssue = "#8093"
	// PendingHooksPlan tracks the remaining terminal hooks (PR 3b).
	PendingHooksPlan = "#7818 group D, PR 3b"
)

// Pending event codes recorded to the instance log. A hook the cloud cannot run
// yet is REPORTED, never silently skipped.
const (
	CodeMarkerRetirePending = "provider_claim_marker_retire_pending"
	CodeHookPending         = "terminal_hook_pending"
)

// ReleaseReason maps a finished run to its claim-release reason, mirroring the
// local terminal mapping (localscheduler.terminalDisposition): the phase decides;
// a completed run that found no work is no-work; a blocked run ended through its
// workflow's own @abort target, so it gave the item back (abandoned).
func ReleaseReason(out frame.Outcome) claimrelease.Reason {
	switch out.Phase {
	case journal.PhaseCompleted:
		if out.Result.NoWork {
			return claimrelease.NoWork.Reason()
		}
		return claimrelease.Completed.Reason()
	case journal.PhaseFailed:
		reason := claimrelease.Failed.Because(out.Result.FailureCode, "")
		if out.Err != nil && out.Result.FailureCode == "" {
			reason = claimrelease.Failed.Because(frame.FailureCode(out.Err), "")
		}
		return reason
	case journal.PhaseEscalated:
		return claimrelease.Escalated.Reason()
	case journal.PhaseAborted:
		if out.Result.Status == engine.StatusBlocked {
			return claimrelease.Abandoned.Reason()
		}
		return claimrelease.Cancelled.Reason()
	}
	return claimrelease.Unknown.Reason()
}

// Finalize releases the run's ledger claims, then REPORTS that the provider
// claim marker still has to be retired (#8093) for each released item. It is
// idempotent: a second call finds no claims and records nothing.
func Finalize(claims Releaser, handle *lease.Handle, record frame.Recorder) func(context.Context, frame.Outcome) error {
	return func(ctx context.Context, out frame.Outcome) error {
		released, err := claims.ReleaseRun(ctx, handle, out.RunID, ReleaseReason(out))
		if err != nil {
			return fmt.Errorf("release claims of run %s: %w", out.RunID, err)
		}
		if len(released) == 0 || record == nil {
			return nil
		}
		items := make([]string, 0, len(released))
		for _, entry := range released {
			items = append(items, entry.ItemID)
		}
		record.AppendBestEffort(stamp(out, journal.Event{
			Type:   journal.EventRunnerAnnotation,
			Reason: "provider claim marker retire pending " + MarkerRetireIssue,
			Runner: map[string]any{"kind": CodeMarkerRetirePending, "issue": MarkerRetireIssue, "items": items, "driver": string(journal.DriverEngine)},
		}))
		return nil
	}
}

// Pending reports one terminal hook the cloud cannot run yet and returns nil, so
// the frame goes on to the next hook. Every run records it: an operator can see
// exactly which instance-level effects a cloud run did not have.
func Pending(record frame.Recorder, hook string) func(context.Context, frame.Outcome) error {
	return func(_ context.Context, out frame.Outcome) error {
		if record != nil {
			record.AppendBestEffort(stamp(out, journal.Event{
				Type:   journal.EventRunnerAnnotation,
				Reason: "terminal hook " + hook + " is not implemented in the cloud yet (" + PendingHooksPlan + ")",
				Runner: map[string]any{"kind": CodeHookPending, "hook": hook, "plan": PendingHooksPlan, "driver": string(journal.DriverEngine)},
			}))
		}
		return nil
	}
}

func stamp(out frame.Outcome, ev journal.Event) journal.Event {
	ev.RunID, ev.Gaggle, ev.Workflow = out.RunID, out.Gaggle, out.Workflow
	return ev
}

// Hooks assembles the cloud frame for one run: the implemented hook (claim
// release) plus a visible pending record for every hook still to come. The
// existing-fix, blocked and failed handlers, escalation, branch cleanup and the
// notification each record themselves as pending when they would have fired.
func Hooks(claims Releaser, handle *lease.Handle, id Identity, record frame.Recorder, claimedItems func(runID string) ([]string, error)) *frame.Hooks {
	pending := func(hook string) func(context.Context, frame.Outcome) error { return Pending(record, hook) }
	outcomeOf := func(runID string) frame.Outcome {
		return frame.Outcome{RunID: runID, Gaggle: id.Gaggle, Workflow: id.Workflow}
	}
	return &frame.Hooks{
		RepoRef:      id.RepoRef,
		ClaimedItems: claimedItems,
		ExistingFix: func(ctx context.Context, o terminalhooks.ExistingFixOutcome) error {
			return pending("existing-fix (strip ready labels)")(ctx, outcomeOf(o.RunID))
		},
		Blocked: func(ctx context.Context, o terminalhooks.BlockedOutcome) error {
			return pending("blocked (park the item, record blockers)")(ctx, outcomeOf(o.RunID))
		},
		Failed: func(ctx context.Context, o terminalhooks.FailedOutcome) error {
			return pending("failed (failure trace, failure streak)")(ctx, outcomeOf(o.RunID))
		},
		Escalate: func(ctx context.Context, out frame.Outcome, _, _, _ string) error {
			return pending("escalation comment")(ctx, out)
		},
		Prepare:  pending("prepare (branch cleanup, aborted-run label)"),
		Notify:   pending("notify (circuit breaker, terminal notification)"),
		Finalize: Finalize(claims, handle, record),
		Record:   record,
	}
}
