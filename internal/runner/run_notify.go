package runner

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

func terminalGateNotificationReason(machine *workflow.Machine, gr gate.Result) (string, bool) {
	// An escalation still notifies the driving issue when the gate's escalate
	// control branch routes disposition work (a parking stage) before the
	// terminal, rather than naming @escalate directly: the repass-attempt count
	// and gate attribution are the point of the notification, and keying only
	// on the control-branch targets would silently drop them.
	if gr.Target != workflow.TargetAbort && gr.Target != workflow.TargetEscalate && !gr.Escalated {
		return "", false
	}
	// An escalation-control task that runs the configured issue close-out
	// operation owns the human-facing disposition. Merely being the escalation
	// branch target is insufficient: custom cleanup tasks may not notify.
	if gr.Escalated && machine != nil {
		if g, ok := machine.Gate(gr.Gate); ok {
			if target, configured := workflow.BranchTarget(g, workflow.BranchEscalate); configured &&
				target == gr.Target && !workflow.IsReservedAnyTarget(target) {
				if task, ok := machine.Task(target); ok && taskOwnsEscalationNotification(task) {
					return "", false
				}
			}
		}
	}
	if gr.Escalated {
		if gr.DuplicateDiff {
			reason := gr.Reason
			if reason == "" {
				reason = gate.ReasonUnchangedRepass
			}
			if gr.RepassCause != nil {
				return reason + ": " + gr.RepassCause.String() + "; the implementer produced no change in response", true
			}
			return reason + ": repass produced a diff identical to the immediately prior attempt", true
		}
		// #3375: an evidence-rejection escalation is not budget exhaustion —
		// no repass was ever charged. Report what actually stopped the run,
		// carrying the synthesized rationale's rejection count and cause.
		if gr.Reason == gate.ReasonRemediationEvidenceNotInspected {
			detail := "the remediation stage never inspected the required failure evidence"
			if gr.Verdict != nil {
				if rationale := strings.TrimSpace(gr.Verdict.Rationale); rationale != "" {
					// The synthesized rationale is already runner-attributed;
					// the reason code carries that, so don't say it twice.
					detail = strings.TrimPrefix(rationale, "runner: ")
				}
			}
			return gr.Reason + ": " + detail, true
		}
		return "repass budget exhausted", true
	}

	reason := fmt.Sprintf("gate %s resolved %s -> %s", gr.Gate, gr.Outcome, gr.Target)
	if gr.Verdict != nil {
		rationale := strings.TrimSpace(gr.Verdict.Rationale)
		if rationale == "" {
			rationale = strings.TrimSpace(gr.Verdict.Summary)
		}
		if rationale != "" {
			reason += ": " + rationale
		}
	}
	return reason, true
}

func taskOwnsEscalationNotification(task apiv1.Task) bool {
	return task.Type == apiv1.TaskDeterministic && task.Run != nil &&
		len(task.Run.Command) >= 2 &&
		task.Run.Command[0] == "goobers" &&
		task.Run.Command[1] == "issue-close-out"
}

func (r *Runner) notifyTerminalGate(ctx context.Context, jr *journal.Run, runID string, repoRef apiv1.RepoRef, item *apiv1.BacklogItem, gr gate.Result, reason string) error {
	if r.cfg.Escalation == nil {
		return nil
	}
	itemIDs, err := r.terminalGateItemIDs(runID, item)
	if err != nil {
		// The claim-ledger fallback failed — journal and swallow, mirroring the
		// NotifyEscalated best-effort contract below. Surfacing the escalation is
		// best-effort; the run must still reach its terminal phase.
		if aerr := jr.Append(journal.Event{
			Type:  journal.EventError,
			Gate:  gr.Gate,
			Error: journal.ErrorDetailFor("gate_terminal_item_resolution_failed", err),
		}); aerr != nil {
			return fmt.Errorf("runner: journal terminal item resolution failure for gate %q: %w", gr.Gate, aerr)
		}
		return nil
	}
	seq := jr.Seq()
	ctx = withRunnerAttributionTask(ctx, gr.Gate)
	for _, itemID := range itemIDs {
		if err := r.cfg.Escalation.NotifyEscalated(ctx, providerRepositoryRef(repoRef), itemID, runID, seq, gr, reason); err != nil {
			if aerr := jr.Append(journal.Event{
				Type:  journal.EventError,
				Gate:  gr.Gate,
				Error: journal.ErrorDetailFor("gate_terminal_notification_failed", err),
			}); aerr != nil {
				return fmt.Errorf("runner: journal terminal notification failure for gate %q: %w", gr.Gate, aerr)
			}
		}
	}
	return nil
}

func providerRepositoryRef(repo apiv1.RepoRef) providers.RepositoryRef {
	return providers.RepositoryRef{
		Provider: providers.ProviderKind(repo.Provider),
		Owner:    repo.Owner,
		Name:     repo.Name,
	}
}

// terminalGateItemIDs resolves the driving backlog item(s) an escalation
// comment should post to. A run started with an Item snapshot (in.Item, e.g. an
// item-triggered dispatch) uses it directly. A run started without one —
// scheduled/fan-out implementation runs, which self-select their item mid-run so
// in.Item is always nil (#796) — falls back to the claim ledger via the
// configured ClaimedItems resolver, exactly as buildBlockedHandler does for the
// blocked path. Nil resolver or no claims yields no ids: nothing to comment on,
// the prior behavior for a producer run with no driving issue.
func (r *Runner) terminalGateItemIDs(runID string, item *apiv1.BacklogItem) ([]string, error) {
	if item != nil && item.ID != "" {
		return []string{item.ID}, nil
	}
	if r.cfg.ClaimedItems == nil {
		return nil, nil
	}
	return r.cfg.ClaimedItems(runID)
}

// notifyBlocked invokes the configured Blocked handler (#544/#545/#552).
// A handler error is journaled (blocked_handling_failed) and swallowed — the
// run must still reach its terminal phase; the recording/parking is
// best-effort, mirroring notifyTerminalGate. Only a journal-write failure is
// returned (a journal that cannot be written is fatal, §2.6).
func (r *Runner) notifyBlocked(ctx context.Context, jr *journal.Run, o BlockedOutcome) error {
	if r.cfg.Blocked == nil {
		return nil
	}
	if err := r.cfg.Blocked(ctx, o); err != nil {
		if aerr := jr.Append(journal.Event{
			Type: journal.EventError, Stage: o.Stage,
			Error: journal.ErrorDetailFor("blocked_handling_failed", err),
		}); aerr != nil {
			return fmt.Errorf("runner: journal blocked-handling failure for %q: %w", o.Stage, aerr)
		}
	}
	return nil
}

// notifyBlockedEscalation surfaces a blocked stage through the same escalation
// notifier used by terminal gates. Provider and claim-resolution failures are
// journaled and swallowed so notification cannot prevent terminal cleanup.
func (r *Runner) notifyBlockedEscalation(ctx context.Context, jr *journal.Run, runID string, item *apiv1.BacklogItem, o BlockedOutcome) error {
	return r.notifyStageEscalation(ctx, jr, runID, item, o.RepoRef, o.Stage, o.Reason)
}

// notifyStageEscalation posts a stage-attributed escalation comment on the
// run's driving item(s). Shared by the blocked terminal (#544) and the
// non-retryable disposition terminal (#415/#3363) — for the latter, the
// stage's own stated reason IS the deliverable (a verified refusal's
// citation), so it must reach the issue rather than live only in the run
// journal on a pod disk. Provider and claim-resolution failures are journaled
// and swallowed so notification cannot prevent terminal cleanup.
func (r *Runner) notifyStageEscalation(ctx context.Context, jr *journal.Run, runID string, item *apiv1.BacklogItem, repoRef apiv1.RepoRef, stage, reason string) error {
	if r.cfg.Escalation == nil {
		return nil
	}
	itemIDs, err := r.terminalGateItemIDs(runID, item)
	if err != nil {
		if aerr := jr.Append(journal.Event{
			Type:  journal.EventError,
			Stage: stage,
			Error: journal.ErrorDetailFor("stage_terminal_item_resolution_failed", err),
		}); aerr != nil {
			return fmt.Errorf("runner: journal terminal item resolution failure for stage %q: %w", stage, aerr)
		}
		return nil
	}
	seq := jr.Seq()
	ctx = withRunnerAttributionTask(ctx, stage)
	for _, itemID := range itemIDs {
		if err := r.cfg.Escalation.NotifyStageEscalated(ctx, providerRepositoryRef(repoRef), itemID, runID, seq, stage, reason); err != nil {
			if aerr := jr.Append(journal.Event{
				Type:  journal.EventError,
				Stage: stage,
				Error: journal.ErrorDetailFor("stage_terminal_notification_failed", err),
			}); aerr != nil {
				return fmt.Errorf("runner: journal terminal notification failure for stage %q: %w", stage, aerr)
			}
		}
	}
	return nil
}

// withRunAttribution carries the run's durable identity to every daemon-side
// provider write made on its behalf (#5178). Start, Resume and RerunStage all
// attach it, so a resumed or rerun run's terminal handling can still satisfy
// the daemon-write attribution guard.
func withRunAttribution(ctx context.Context, gaggle, workflow, runID string) context.Context {
	return providers.WithAttributionContext(ctx, providers.Attribution{
		Schema: 1, Goobers: true,
		Gaggle: gaggle, Workflow: workflow,
		Goober: "runner", Run: runID,
	})
}

func withRunnerAttributionTask(ctx context.Context, task string) context.Context {
	attribution, ok := providers.AttributionFromContext(ctx)
	if !ok {
		return ctx
	}
	attribution.Task = task
	attribution.Goober = "runner"
	return providers.WithAttributionContext(ctx, attribution)
}

// notifyRateLimited invokes the configured RateLimited handler (#712). A
// handler error is journaled (rate_limited_handling_failed) and swallowed —
// unlike notifyBlocked, this never gates the run's own terminal phase, so
// only a journal-write failure is returned.
func (r *Runner) notifyRateLimited(ctx context.Context, jr *journal.Run, o RateLimitedOutcome) error {
	if r.cfg.RateLimited == nil {
		return nil
	}
	if err := r.cfg.RateLimited(ctx, o); err != nil {
		if aerr := jr.Append(journal.Event{
			Type: journal.EventError, Stage: o.Stage,
			Error: journal.ErrorDetailFor("rate_limited_handling_failed", err),
		}); aerr != nil {
			return fmt.Errorf("runner: journal rate-limited-handling failure for %q: %w", o.Stage, aerr)
		}
	}
	return nil
}

// notifyFailed invokes the configured Failed handler (#1054) at a terminal
// PhaseFailed transition — after the run_failed cause is journaled and before
// the run's terminal run.finished, mirroring notifyBlocked's ordering so the
// claim ledger still holds this run's claims (FinalizeTerminal releases them
// only after). A handler error is journaled (failed_handling_failed) and
// swallowed — the run must still reach its terminal phase; leaving the trace is
// best-effort. Only a journal-write failure is returned (a journal that cannot
// be written is fatal, §2.6).
func (r *Runner) notifyFailed(ctx context.Context, jr *journal.Run, o FailedOutcome) error {
	if r.cfg.Failed == nil {
		return nil
	}
	if err := r.cfg.Failed(ctx, o); err != nil {
		if aerr := jr.Append(journal.Event{
			Type: journal.EventError, Stage: o.Stage,
			Error: journal.ErrorDetailFor("failed_handling_failed", err),
		}); aerr != nil {
			return fmt.Errorf("runner: journal failed-handling failure for run %q: %w", o.RunID, aerr)
		}
	}
	return nil
}

// outputRateLimitReset parses the rateLimitReset RFC3339 timestamp a stage
// writes into its declared result file on a github_rate_limited failure
// (failProviderStage, cmd/goobers/providercmd.go, #614). Returns zero/false
// when the key is absent, not a string, or not parseable — a rate-limited
// failure whose reset couldn't be recovered simply skips notifyRateLimited
// rather than surfacing a second, unrelated parse error.
func outputRateLimitReset(outputs map[string]interface{}) (time.Time, bool) {
	v, ok := outputs["rateLimitReset"]
	if !ok {
		return time.Time{}, false
	}
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// blockedReason condenses a blocked result's own explanation for the journal's
// blocked_by_agent cause event: the structured error detail when present
// (code-prefixed, so an agent's DEPENDENCY_NOT_MET survives into the run-level
// record), else the summary, else a fixed marker so the event is never empty.
func blockedReason(result apiv1.ResultEnvelope) string {
	if result.Error != nil && result.Error.Message != "" {
		if result.Error.Code != "" {
			return result.Error.Code + ": " + result.Error.Message
		}
		return result.Error.Message
	}
	if s := strings.TrimSpace(result.Summary); s != "" {
		return s
	}
	return "stage reported blocked with no error detail"
}

// failureCauseFrom extracts the bare (code, message) pair a business
// ResultFailure carries (issue #710) — code feeds Result.FailureCode directly
// (the scheduler/daemon echo's own "(stage: CODE)" suffix already shows the
// code, so folding it into message too would be redundant there); message is
// the bare stage-reported text, code-prefixed separately by the caller only
// where that reads better (the run_failed journal event, matching #545's
// blockedReason convention). Falls back to a fixed marker so neither is ever
// empty — docs/stage-contract.md requires "error" on every failure result,
// but a stage that violates that (a bug in the executor, not the contract)
// must still produce a describable cause rather than an empty one.
func failureCauseFrom(e *apiv1.ErrorInfo) (code, message string) {
	if e == nil || e.Message == "" {
		return "", "stage reported failure with no error detail"
	}
	return e.Code, e.Message
}

// parseBlockedBy extracts blocking issue numbers from the documented
// outputs.blockedBy convention (docs/stage-contract.md): a scalar string of
// comma-separated issue numbers — the envelope schema admits only scalars in
// outputs, which is exactly why live blocked results that tried a structured
// array got schema-rejected and burned an attempt (#545). Lenient on input
// shape (tolerates "#" prefixes, whitespace, a bare JSON number), strict on
// content: only all-digit tokens survive, deduplicated in first-seen order.
// Nil when the key is absent or nothing parseable remains.
func parseBlockedBy(outputs map[string]interface{}) []string {
	v, ok := outputs[OutputBlockedBy]
	if !ok || v == nil {
		return nil
	}
	var raw string
	switch n := v.(type) {
	case string:
		raw = n
	case float64:
		raw = strconv.FormatInt(int64(n), 10)
	case int:
		raw = strconv.Itoa(n)
	default:
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, tok := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' }) {
		tok = strings.TrimPrefix(strings.TrimSpace(tok), "#")
		if tok == "" || seen[tok] {
			continue
		}
		allDigits := true
		for _, r := range tok {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if !allDigits {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}

// OutputBlockedBy is the documented ResultEnvelope output key a blocked stage
// references its blocking issue numbers through — comma-separated numbers in
// a single scalar string (outputs are scalar-only by schema). Shared by the
// runner's parse (above) and the instructions/docs that teach producers the
// convention.
const OutputBlockedBy = "blockedBy"

// SelfBlockerDroppedKind tags the runner.annotation recording that a stage
// named its own driving item in outputs.blockedBy and the self-reference was
// dropped before persistence (#2961).
const SelfBlockerDroppedKind = "blocked_by.self_reference_dropped"

// FilterSelfBlockers removes blockers naming the driving item itself, which an
// item can never depend on (#2961). Admitting a self-reference writes a
// one-node self-edge into scheduler/blocked.json that the cycle detector then
// correctly reports as a circular dependency, parking the issue for human
// resolution over a dependency that does not exist. The detector is not the
// bug — the missing validation at the point model-authored blocker output is
// admitted is, so this filters on the way in and leaves persisted-graph
// self-loop handling intact for legacy or corrupt records.
//
// Both sides are normalized (repository qualifier and "#" prefix stripped) so
// "#441", "owner/repo#441" and "441" all match item 441. Matching is exact
// after normalization: a pull-request item ("pr/536") is never considered
// self-blocked by issue 536, which is a legitimate dependency.
// Returns the blockers to keep and those dropped, in first-seen order.
func FilterSelfBlockers(blockers []string, itemID string) (kept, dropped []string) {
	self := normalizeBlockerToken(itemID)
	if self == "" {
		return blockers, nil
	}
	for _, blocker := range blockers {
		if normalizeBlockerToken(blocker) == self {
			dropped = append(dropped, blocker)
			continue
		}
		kept = append(kept, blocker)
	}
	return kept, dropped
}

// normalizeBlockerToken reduces an item id or blocker reference to the bare
// identifier both sides of a self-comparison can be keyed on, mirroring the
// CLI's normalizeBlockedReference: drop any repository qualifier ahead of the
// last "#", then any leading "#".
func normalizeBlockerToken(raw string) string {
	raw = strings.TrimSpace(raw)
	if separator := strings.LastIndex(raw, "#"); separator >= 0 {
		raw = raw[separator+1:]
	}
	return strings.TrimSpace(strings.TrimPrefix(raw, "#"))
}
