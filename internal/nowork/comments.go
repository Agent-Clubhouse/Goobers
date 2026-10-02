package nowork

import (
	"fmt"
	"strings"

	"github.com/goobers/goobers/providers"
)

// StreakThreshold is how many no-work terminals on the SAME item park it
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
const StreakThreshold = 3

// ParkMarker prefixes the park comment so an operator grepping an issue
// can find why the item left the ready pool. It is a locator, not a dedupe
// key: nothing reads it back, and a park that is re-applied posts a fresh
// comment. That is tolerable because parking removes the item from every
// selector, so a re-claim — and hence a second park — is unlikely.
const ParkMarker = "<!-- goobers:no-work-park -->"

// VerdictMarker prefixes the comment recording each no-work verdict
// below the park threshold (#5643), and ContradictionMarker the comment
// flagging a later run that disagreed with a recorded verdict. Locators only,
// like ParkMarker.
const (
	VerdictMarker       = "<!-- goobers:no-work-verdict -->"
	ContradictionMarker = "<!-- goobers:no-work-verdict-contradicted -->"
)

// VerdictConflict renders the line flagging a no-work verdict that
// disagrees with the one recorded before it (#5643), "" when they agree or
// either side did not classify its verdict.
func VerdictConflict(previous, recorded Record) string {
	if previous.Count == 0 || previous.Verdict == "" || recorded.Verdict == "" || previous.Verdict == recorded.Verdict {
		return ""
	}
	return fmt.Sprintf("**This verdict contradicts the previous one:** run `%s` recorded `%s`, this run recorded `%s`.",
		previous.RunID, previous.Verdict, recorded.Verdict)
}

// ParkComment renders the human-visible explanation for a park. It ends
// on an explicit question because a parked item's comment is the only thing
// standing between a human and an item that silently left the ready pool —
// the same contract issuecloseout.go enforces for its own park comments.
func ParkComment(recorded Record, contradiction, runID, runURL string) string {
	var b strings.Builder
	b.WriteString(ParkMarker)
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

// VerdictComment records one no-work verdict on the issue (#5643), so a
// verdict below the park threshold is auditable where the next decision-maker
// looks instead of living only in one run's journal.
func VerdictComment(recorded Record, contradiction, runID, runURL string) string {
	var b strings.Builder
	b.WriteString(VerdictMarker)
	fmt.Fprintf(&b, "\n\n**The `%s` stage concluded there is nothing to do on this item", recorded.Stage)
	if recorded.Verdict != "" {
		fmt.Fprintf(&b, " (`%s`)", recorded.Verdict)
	}
	b.WriteString(".**\n\n")
	writeNoWorkVerdictDetail(&b, recorded, contradiction)
	writeNoWorkRunLink(&b, "Run", runID, runURL)
	fmt.Fprintf(&b, "This is `no-work` verdict %d of %d before the item is parked for a human. "+
		"The next run on this item is handed this verdict; a run that disagrees with it is flagged here.",
		recorded.Count, StreakThreshold)
	return b.String()
}

// ContradictionComment flags a run that completed without a no-work
// verdict on an item whose last recorded verdict said there was nothing to do
// (#5643). One of the two conclusions is wrong, and nothing else would show it.
func ContradictionComment(cleared Record, runID, runURL string) string {
	var b strings.Builder
	b.WriteString(ContradictionMarker)
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
func writeNoWorkVerdictDetail(b *strings.Builder, recorded Record, contradiction string) {
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
