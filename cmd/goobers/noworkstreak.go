package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// noWorkStreakThreshold is how many no-work terminals on the SAME item park it
// for a human. Three mirrors escalationnotify.FailureStreakThreshold: the
// point is not to be clever about the number but to make an unactionable item
// stop occupying a lane long before a human would otherwise notice. #5379's
// incident ran to 28 claims over two days.
//
// Precisely, the streak counts no-work COMPLETIONS with no productive
// completion in between. Only a completed terminal settles this counter, so an
// escalated or aborted terminal neither increments nor clears it — an item can
// therefore reach the threshold across runs that were not strictly
// consecutive. That matches the scope decision, which requires a reset on
// productive completion and says nothing about other terminals, but it is why
// the park comment must not claim "three times in a row".
const noWorkStreakThreshold = 3

// noWorkParkMarker prefixes the park comment so an operator grepping an issue
// can find why the item left the ready pool. It is a locator, not a dedupe
// key: nothing reads it back, and a park that is re-applied posts a fresh
// comment. That is tolerable because parking removes the item from every
// selector, so a re-claim — and hence a second park — is unlikely.
const noWorkParkMarker = "<!-- goobers:no-work-park -->"

// noWorkVerdictMarker prefixes the comment recording each no-work verdict
// below the park threshold (#5643), and noWorkContradictionMarker the comment
// flagging a later run that disagreed with a recorded verdict. Locators only,
// like noWorkParkMarker.
const (
	noWorkVerdictMarker       = "<!-- goobers:no-work-verdict -->"
	noWorkContradictionMarker = "<!-- goobers:no-work-verdict-contradicted -->"
)

// noWorkTerminal describes a completed run that ended on a no-work verdict:
// which stage declined, the rationale it recorded, its own classification of
// the verdict (for example "already-fixed") and the evidence it cited (for
// example an existing commit). Each is empty when the stage journaled none.
type noWorkTerminal struct {
	stage    string
	reason   string
	verdict  string
	evidence string
}

// noWorkTerminalForRun reports whether a COMPLETED run ended because its
// TERMINAL stage answered no-work, and carries that stage's recorded
// rationale.
//
// Why the run's own terminal disposition cannot answer this: finishNoWork
// (internal/runner/run.go) marks a run journal.RunDispositionNoWork only when
// `ws.steps == 1` or a fan-in settled every branch empty. The implementation
// workflow reaches `implement` at step 3, so a no-work verdict there completes
// the run as RunDispositionProduced — indistinguishable, at the terminal, from
// a run that actually opened a pull request. That is precisely why #5379's
// loop was invisible: 28 consecutive claims all reported as productive.
//
// The stage.finished event is the honest signal. It carries the executor's
// literal apiv1.ResultNoWork status (internal/runner/run.go's runTask appends
// `Status: string(result.Status)`), and it carries the stage's Outputs, which
// is where a rationale lives.
//
// Two filters make this the run's verdict rather than merely A no-work event
// somewhere in its history, and BOTH are load-bearing:
//
//   - `event.Stage == finalState`. finalState is the stage the run actually
//     ended on, handed to the terminal notifier. Without this, a run that
//     no-worked at `implement`, repassed through a gate (#5107) and then went
//     on to produce a pull request still looks like a no-work run, because the
//     stale event remains in the journal forever.
//   - `event.Branch == 0`. Parallel branch stages append to the SAME run
//     journal (internal/runner/parallel_run.go stamps ev.Branch and forwards
//     to the run journal), and a branch that returns no-work ends only that
//     branch — its siblings keep running and the join proceeds. Counting a
//     branch's verdict as the run's would park an item whose fan-out run
//     produced real findings on every other branch.
//
// Scanning for the newest matching event rather than the first is what makes a
// repass converge on its final attempt.
func noWorkTerminalForRun(l instance.Layout, runID, finalState string) (noWorkTerminal, bool, error) {
	if finalState == "" {
		return noWorkTerminal{}, false, nil
	}
	reader, err := journal.OpenReadOnly(filepath.Join(l.RunsDir(), runID))
	if err != nil {
		return noWorkTerminal{}, false, fmt.Errorf("open journal for run %q: %w", runID, err)
	}
	events, err := reader.Events()
	if err != nil {
		return noWorkTerminal{}, false, fmt.Errorf("read journal for run %q: %w", runID, err)
	}
	var found noWorkTerminal
	var ok bool
	for _, event := range events {
		if event.Type != journal.EventStageFinished || event.Branch != 0 {
			continue
		}
		if event.Stage != finalState || event.Status != string(apiv1.ResultNoWork) {
			continue
		}
		found = noWorkTerminal{
			stage:    event.Stage,
			reason:   noWorkReasonFromOutputs(event.Outputs),
			verdict:  stringOutput(event.Outputs, "status"),
			evidence: stringOutput(event.Outputs, "existingCommit"),
		}
		ok = true
	}
	return found, ok, nil
}

// noWorkReasonFromOutputs extracts the rationale a stage recorded alongside a
// no-work verdict. Deterministic stages already write this key
// (cmd/goobers/backlogquery.go's writeNoWorkResult), and internal/diagnostics
// already allowlists it for surfacing, so an agentic stage that emits the same
// key is picked up by both without further plumbing.
//
// An agentic stage that names its rationale "reason" instead (#5643's
// already-fixed verdicts did) is read as a fallback, so the verdict it wrote
// is not discarded for using the other spelling.
func noWorkReasonFromOutputs(outputs map[string]any) string {
	if reason := stringOutput(outputs, "noWorkReason"); reason != "" {
		return reason
	}
	return stringOutput(outputs, "reason")
}

// stringOutput returns a stage output's trimmed string value, "" when the key
// is absent or not a string.
func stringOutput(outputs map[string]any, key string) string {
	text, _ := outputs[key].(string)
	return strings.TrimSpace(text)
}

// settleNoWorkStreak is the completed-terminal entry point: it decides whether
// this run counted as work and routes to the increment or the reset.
//
// It is kept as one call so the terminal notifier gains a single branch
// rather than the classification logic itself — the same decomposition idiom
// #5107 used for noWorkRepassOutcome, and what the complexity gate rewards.
//
// A journal that cannot be read leaves the streak ALONE — it neither
// increments nor resets.
//
// Resetting would be the more obvious "fail safe", but it is not safe here: a
// transient read fault on a run that really was no-work would silently discard
// an accumulated streak, and a recurrent one (a pruned run directory, a
// truncated journal, an I/O error) would make the park permanently unreachable
// with no diagnostic. Incrementing would be worse still, walking an item
// toward a park on no evidence. Doing nothing preserves whatever the last
// readable terminal established.
//
// The read error is deliberately NOT propagated, matching the posture its
// immediate sibling in escalationnotify.Policy.TerminalNotifier takes
// (`attributedCtx, _ := AttributionContextForRun(...)`): a terminal whose run
// directory has been pruned, or which never had a journal, is an ordinary
// condition on this path and must not be reported as a failed terminal
// notification. Errors from the state-plane work below ARE returned, because
// those represent a streak the instance failed to account for.
func settleNoWorkStreak(
	ctx context.Context,
	poster gate.Commenter,
	l instance.Layout,
	runID, finalState, runURL string,
) error {
	terminal, isNoWork, err := noWorkTerminalForRun(l, runID, finalState)
	if err != nil {
		return nil
	}
	if isNoWork {
		return applyNoWorkStreak(ctx, poster, l, runID, terminal, runURL)
	}
	return resetNoWorkStreaks(ctx, poster, l, runID, runURL)
}

// applyNoWorkStreak increments the repeated-no-work counter for every item the
// completed run held, and parks the item once it reaches the threshold.
//
// Ordering mirrors the escalationnotify circuit breaker deliberately: the
// authoritative state update happens FIRST and the provider mutation second,
// so a park that cannot reach the provider still leaves a durable record that
// the protection was owed. #5379's scope decision requires "idempotent
// finalization" — re-running this for an already-parked item is safe because
// the provider mutation is a label swap that converges, and because a terminal notification that is
// retried after a partial failure re-reads the persisted count rather than
// recomputing it from provider state.
// Only a run holding EXACTLY ONE claimed item is counted. A no-work verdict
// says "this run found nothing to do"; when a run holds a batch, nothing in
// that verdict attributes the conclusion to any particular member of the
// batch, so charging it to all of them is simply wrong.
//
// This is not hypothetical. backlog-curation claims up to 20 items per run
// (maxItems: "20") and its agentic `curate` stage short-circuits to a
// completed terminal without ever reaching `release-claim`, so all 20 claims
// are still held here. Charging each of them would stamp goobers:needs-human
// across 20 backlog items per no-work curation run — items that do not even
// carry goobers:ready, so no re-offer loop is being broken and the park is
// pure damage. Worse, that workflow's own selection filters on park labels, so
// the items would be permanently excluded from curation thereafter.
//
// The escalationnotify circuit breaker fans out across every claimed item, but
// it is only reached from failure and escalation terminals where "everything
// this run held is implicated" is defensible. On a completed terminal it is
// not.
//
// #5379's own loop is a single-item implementation run, so the narrow rule
// covers the reported defect exactly.
func applyNoWorkStreak(
	ctx context.Context,
	poster gate.Commenter,
	l instance.Layout,
	runID string,
	terminal noWorkTerminal,
	runURL string,
) error {
	items, err := claimedItemsForRun(l, runID)
	if err != nil {
		return err
	}
	if len(items) != 1 {
		return nil
	}
	repoRef, itemID := items[0].Repo, items[0].ItemID
	recorded, previous, err := incrementNoWorkStreak(ctx, l, repoRef, itemID, runID, terminal)
	if err != nil {
		return fmt.Errorf("persist no-work streak on %s#%s: %w", repoRef.Name, itemID, err)
	}
	contradiction := noWorkVerdictConflict(previous, recorded)
	if recorded.Count < noWorkStreakThreshold {
		// #5643: every verdict below the park threshold is written to the
		// issue too, so it is never left looking like an item nobody read.
		comment := noWorkVerdictComment(recorded, contradiction, runID, runURL)
		if _, err := poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repoRef, ID: itemID, Comment: comment,
		}); err != nil {
			return fmt.Errorf("record no-work verdict on %s#%s: %w", repoRef.Name, itemID, err)
		}
		return nil
	}
	comment := noWorkParkComment(recorded, contradiction, runID, runURL)
	if _, err := poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository:   repoRef,
		ID:           itemID,
		Comment:      comment,
		AddLabels:    []string{providers.LabelNeedsHuman},
		RemoveLabels: []string{providers.LabelReady},
	}); err != nil {
		return fmt.Errorf("park repeated no-work on %s#%s: %w", repoRef.Name, itemID, err)
	}
	return nil
}

// noWorkVerdictConflict renders the line flagging a no-work verdict that
// disagrees with the one recorded before it (#5643), "" when they agree or
// either side did not classify its verdict.
func noWorkVerdictConflict(previous, recorded noWorkStreakRecord) string {
	if previous.Count == 0 || previous.Verdict == "" || recorded.Verdict == "" || previous.Verdict == recorded.Verdict {
		return ""
	}
	return fmt.Sprintf("**This verdict contradicts the previous one:** run `%s` recorded `%s`, this run recorded `%s`.",
		previous.RunID, previous.Verdict, recorded.Verdict)
}

// resetNoWorkStreaks clears the repeated-no-work counter for every item a
// PRODUCTIVE completion held.
//
// The reset itself depends on nothing but the state plane. #5379's scope
// decision requires that "productive completion resets the correct streaks
// even when provider notifications fail", so the one provider call here — the
// #5643 contradiction comment — runs only after every reset has landed, and
// its failure is reported without undoing any of them. That failure is not
// retried: a replayed notification finds the record already cleared, so the
// flag is lost rather than ever blocking or repeating a reset. A stale park comment
// left on an item is cosmetic; a streak that failed to reset would eventually
// park an item that is demonstrably producing work.
func resetNoWorkStreaks(ctx context.Context, poster gate.Commenter, l instance.Layout, runID, runURL string) error {
	items, err := claimedItemsForRun(l, runID)
	if err != nil {
		return err
	}
	var errs []error
	contradicted := map[int]noWorkStreakRecord{}
	for i, item := range items {
		cleared, err := resetNoWorkStreakState(ctx, l, item.Repo, item.ItemID, runID)
		if err != nil {
			errs = append(errs, fmt.Errorf("reset no-work streak on %s#%s: %w", item.Repo.Name, item.ItemID, err))
		} else if cleared.Count > 0 {
			contradicted[i] = cleared
		}
	}
	// #5643: a run that completed without a no-work verdict disagrees with the
	// verdict it just cleared, so say so on the issue — after every reset has
	// landed, so a provider outage can never block one. Single-item runs only,
	// for applyNoWorkStreak's reason: a batch run's completion says nothing
	// about any one member.
	if len(items) == 1 && len(contradicted) == 1 {
		item := items[0]
		comment := noWorkContradictionComment(contradicted[0], runID, runURL)
		if _, err := poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: item.Repo, ID: item.ItemID, Comment: comment,
		}); err != nil {
			errs = append(errs, fmt.Errorf("flag contradicted no-work verdict on %s#%s: %w", item.Repo.Name, item.ItemID, err))
		}
	}
	return errors.Join(errs...)
}

// noWorkParkComment renders the human-visible explanation for a park. It ends
// on an explicit question because a parked item's comment is the only thing
// standing between a human and an item that silently left the ready pool —
// the same contract issuecloseout.go enforces for its own park comments.
func noWorkParkComment(recorded noWorkStreakRecord, contradiction, runID, runURL string) string {
	var b strings.Builder
	b.WriteString(noWorkParkMarker)
	b.WriteString("\n\n**Parked after ")
	fmt.Fprintf(&b, "%d `no-work` verdicts with no productive run in between.**\n\n", recorded.Count)
	fmt.Fprintf(&b, "This item was claimed and the `%s` stage concluded there was nothing to do, "+
		"%d times, with no run producing work in between. Each time the item was released still "+
		"carrying `%s` and became immediately claimable again. Removing `%s` stops that loop.\n\n",
		recorded.Stage, recorded.Count, providers.LabelReady, providers.LabelReady)
	writeNoWorkVerdictDetail(&b, recorded, contradiction)
	writeNoWorkRunLink(&b, "Most recent run", runID, runURL)
	b.WriteString("Is this item actually actionable? If it is, correct or clarify it and re-add `")
	b.WriteString(providers.LabelReady)
	b.WriteString("`; if it is not, please close it.")
	return b.String()
}

// noWorkVerdictComment records one no-work verdict on the issue (#5643), so a
// verdict below the park threshold is auditable where the next decision-maker
// looks instead of living only in one run's journal.
func noWorkVerdictComment(recorded noWorkStreakRecord, contradiction, runID, runURL string) string {
	var b strings.Builder
	b.WriteString(noWorkVerdictMarker)
	fmt.Fprintf(&b, "\n\n**The `%s` stage concluded there is nothing to do on this item", recorded.Stage)
	if recorded.Verdict != "" {
		fmt.Fprintf(&b, " (`%s`)", recorded.Verdict)
	}
	b.WriteString(".**\n\n")
	writeNoWorkVerdictDetail(&b, recorded, contradiction)
	writeNoWorkRunLink(&b, "Run", runID, runURL)
	fmt.Fprintf(&b, "This is `no-work` verdict %d of %d before the item is parked for a human. "+
		"The next run on this item is handed this verdict; a run that disagrees with it is flagged here.",
		recorded.Count, noWorkStreakThreshold)
	return b.String()
}

// noWorkContradictionComment flags a run that completed without a no-work
// verdict on an item whose last recorded verdict said there was nothing to do
// (#5643). One of the two conclusions is wrong, and nothing else would show it.
func noWorkContradictionComment(cleared noWorkStreakRecord, runID, runURL string) string {
	var b strings.Builder
	b.WriteString(noWorkContradictionMarker)
	b.WriteString("\n\n**A later run contradicted a recorded `no-work` verdict on this item.**\n\n")
	fmt.Fprintf(&b, "Run `%s` had concluded at the `%s` stage that there was nothing to do", cleared.RunID, cleared.Stage)
	if cleared.Verdict != "" {
		fmt.Fprintf(&b, " (`%s`)", cleared.Verdict)
	}
	b.WriteString(". This run then completed without a `no-work` verdict, so the two runs disagree about whether this item needed a change.\n\n")
	writeNoWorkVerdictDetail(&b, cleared, "")
	writeNoWorkRunLink(&b, "Contradicting run", runID, runURL)
	b.WriteString("Check which conclusion was right: if the earlier verdict was a false negative, nothing further is needed here.")
	return b.String()
}

// writeNoWorkVerdictDetail writes a recorded verdict's reason, evidence and
// any contradiction line.
func writeNoWorkVerdictDetail(b *strings.Builder, recorded noWorkStreakRecord, contradiction string) {
	if recorded.Reason != "" {
		b.WriteString("Last recorded reason:\n\n> ")
		b.WriteString(strings.ReplaceAll(recorded.Reason, "\n", "\n> "))
		b.WriteString("\n\n")
	} else {
		b.WriteString("The stage recorded no reason for the verdict, so there is no rationale to quote here.\n\n")
	}
	if recorded.Evidence != "" {
		fmt.Fprintf(b, "Cited evidence: `%s`\n\n", recorded.Evidence)
	}
	if contradiction != "" {
		b.WriteString(contradiction)
		b.WriteString("\n\n")
	}
}

func writeNoWorkRunLink(b *strings.Builder, label, runID, runURL string) {
	if runURL != "" {
		fmt.Fprintf(b, "%s: %s\n\n", label, runURL)
	} else if runID != "" {
		fmt.Fprintf(b, "%s: `%s`\n\n", label, runID)
	}
}
