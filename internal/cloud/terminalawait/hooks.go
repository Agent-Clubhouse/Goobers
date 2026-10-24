package terminalawait

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/escalationnotify"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/terminalhooks"
	"github.com/goobers/goobers/internal/terminalhooks/frame"
	"github.com/goobers/goobers/internal/terminalhooks/parking"
	"github.com/goobers/goobers/internal/terminalhooks/providerposter"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
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

// Option extends the cloud frame with hooks that have a real implementation.
type Option func(*options)

type options struct {
	writer  providerwriter.Writer
	parking parking.State
	failed  func(runID string) *escalationnotify.Policy
	notify  func(runID string) *escalationnotify.Policy
}

// WithFailed implements the failed hook: the failure-streak circuit breaker
// (#1054), through the policy policyFor builds for the run (its poster, state and
// terminal_report outbox are the caller's). The provider writes inside it are
// reported as failed while the writer is Pending, but the streak is counted.
func WithFailed(policyFor func(runID string) *escalationnotify.Policy) Option {
	return func(o *options) { o.failed = policyFor }
}

// WithParking implements the existing-fix, escalation and blocked hooks: the
// block records and claimed items come from state, and every provider write goes
// through writer (providerwriter.Pending until the control role holds provider
// credentials, #8093, in which case the writes are reported as failed, never
// silently dropped).
func WithParking(writer providerwriter.Writer, state parking.State) Option {
	return func(o *options) { o.writer, o.parking = writer, state }
}

// WithNotify implements the notify hook: the circuit breaker for an escalated or
// aborted terminal, and the failure and no-work streak resets for a completed
// one (escalationnotify.Policy.Terminal), through the policy policyFor builds
// for the run.
func WithNotify(policyFor func(runID string) *escalationnotify.Policy) Option {
	return func(o *options) { o.notify = policyFor }
}

// Hooks assembles the cloud frame for one run: the implemented hooks (claim
// release, plus those added by options) and a visible pending record for every
// hook still to come.
func Hooks(claims Releaser, handle *lease.Handle, id Identity, record frame.Recorder, claimedItems func(runID string) ([]string, error), opts ...Option) *frame.Hooks {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	pending := func(hook string) func(context.Context, frame.Outcome) error { return Pending(record, hook) }
	outcomeOf := func(runID string) frame.Outcome {
		return frame.Outcome{RunID: runID, Gaggle: id.Gaggle, Workflow: id.Workflow}
	}
	h := &frame.Hooks{
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
	if o.failed != nil || o.notify != nil || (o.writer != nil && o.parking != nil) {
		// The shared policies attribute provider writes to the run; the daemon
		// reads that from the run's journal, the cloud supplies it.
		h.Context = func(ctx context.Context, out frame.Outcome) context.Context {
			return providers.WithAttributionContext(ctx, providers.Attribution{
				Schema: 1, Goobers: true, Gaggle: out.Gaggle, Workflow: out.Workflow, Goober: "runner", Run: out.RunID,
			})
		}
	}
	if o.failed != nil {
		h.Failed = func(ctx context.Context, f terminalhooks.FailedOutcome) error {
			return o.failed(f.RunID).Failed(ctx, f)
		}
	}
	if o.notify != nil {
		h.Notify = func(ctx context.Context, out frame.Outcome) error {
			return o.notify(out.RunID).Terminal(ctx, out.RunID, out.Phase, out.Result.FinalState)
		}
	}
	if o.writer != nil && o.parking != nil {
		h.ExistingFix = func(ctx context.Context, e terminalhooks.ExistingFixOutcome) error {
			return parking.ExistingFix(ctx, providerposter.Poster{Writer: o.writer, RunID: e.RunID}, e)
		}
		h.Blocked = func(ctx context.Context, b terminalhooks.BlockedOutcome) error {
			return parking.Blocked(ctx, providerposter.Poster{Writer: o.writer, RunID: b.RunID}, o.parking, b)
		}
		h.Escalate = func(ctx context.Context, out frame.Outcome, itemID, stage, reason string) error {
			notifier := gate.EscalationNotifier{Poster: providerposter.Poster{Writer: o.writer, RunID: out.RunID}}
			repo := providers.RepositoryRef{Provider: providers.ProviderKind(id.RepoRef.Provider), Owner: id.RepoRef.Owner, Name: id.RepoRef.Name}
			// The run-notification marker (run, seq) makes a repeated post a no-op;
			// a run escalates once, so the sequence is fixed.
			return notifier.NotifyStageEscalated(ctx, repo, itemID, out.RunID, 0, stage, reason)
		}
	}
	return h
}
