package childworkflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workflowcluster"
)

// AdmissionHelp is the operator-facing human-admission review command's help.
const AdmissionHelp = "Usage: goobers workflow child-admission [--approve <child-id> | --reject <child-id>] [--actor <identity>] [--note <text>] [--json] [path]\n\n" +
	"Review generated child workflows held for human admission approval because\n" +
	"the parent's childWorkflows.requireHumanApprovalForOutOfDistribution policy\n" +
	"(Labs) found them outside every cluster of the frozen model. A held child keeps\n" +
	"durable custody but is never dispatched until approved.\n\n" +
	"Without --approve or --reject, list pending children with their pinned novelty\n" +
	"evidence. --approve releases the child to ordinary dispatch and journals the\n" +
	"approval in its lineage; --reject settles it as a failed child result that the\n" +
	"parent observes. Decisions are durable and immutable; repeating one is a no-op.\n" +
	"--actor defaults to the invoking user: GITHUB_ACTOR, USER or USERNAME, then the\n" +
	"OS account. The actor is self-asserted, matching the trusted-local model:\n" +
	"anyone who can open the instance's local queue can decide, and the actor is\n" +
	"recorded for audit, not authenticated. Operates on the instance's local queue\n" +
	"(default path \".\").\n\n" +
	"Exit codes: 0 = listed or decided, 1 = decision refused, 2 = usage/IO error.\n"

// AdmissionStore is the durable queue surface the review command needs.
type AdmissionStore interface {
	PendingChildAdmissions(ctx context.Context, after string, limit int) ([]triggerqueue.ChildAdmissionReview, error)
	DecideChildAdmission(ctx context.Context, decision triggerqueue.ChildAdmissionDecision, now time.Time) (triggerqueue.ChildAdmissionReview, error)
}

// AdmissionCLI supplies host effects so command parsing stays testable.
type AdmissionCLI struct {
	// Open returns the instance queue rooted at path and its release function.
	Open         func(root string) (AdmissionStore, func(), error)
	DefaultActor func() (string, error)
	Now          func() time.Time
}

// AdmissionReport is the stable --json shape for one held child.
type AdmissionReport struct {
	ChildID         string                `json:"childId"`
	Gaggle          string                `json:"gaggle"`
	ParentRunID     string                `json:"parentRunId"`
	StageOccurrence string                `json:"stageOccurrence"`
	InvocationKey   string                `json:"invocationKey"`
	State           string                `json:"state"`
	Reason          string                `json:"reason"`
	Novelty         *journal.ChildNovelty `json:"novelty,omitempty"`
	Summary         string                `json:"summary"`
	RequestedAt     time.Time             `json:"requestedAt"`
	DecidedAt       *time.Time            `json:"decidedAt,omitempty"`
	Actor           string                `json:"actor,omitempty"`
	Note            string                `json:"note,omitempty"`
}

const maxAdmissionListing = 1000

// Run parses args and lists or decides held child admissions.
func (c AdmissionCLI) Run(args []string, fs *flag.FlagSet, stdout, stderr io.Writer) int {
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, AdmissionHelp) }
	approve := fs.String("approve", "", "child ID to approve for dispatch")
	reject := fs.String("reject", "", "child ID to reject before dispatch")
	actor := fs.String("actor", "", "audit identity (default: the invoking user)")
	note := fs.String("note", "", "optional audit note recorded with the decision")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 1 || (*approve != "" && *reject != "") || ((*actor != "" || *note != "") && *approve == "" && *reject == "") {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	store, release, err := c.Open(root)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: open child queue: %v\n", err)
		return 2
	}
	defer release()
	ctx := context.Background()
	if *approve == "" && *reject == "" {
		pending, err := store.PendingChildAdmissions(ctx, "", maxAdmissionListing)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: list held children: %v\n", err)
			return 2
		}
		reports := make([]AdmissionReport, 0, len(pending))
		for _, review := range pending {
			reports = append(reports, NewAdmissionReport(review))
		}
		return writeAdmissionReports(stdout, stderr, reports, *asJSON, "No child workflows are awaiting human admission approval.")
	}
	decision := triggerqueue.ChildAdmissionDecision{ChildID: *approve, Approve: true, Actor: *actor, Note: *note}
	if *reject != "" {
		decision = triggerqueue.ChildAdmissionDecision{ChildID: *reject, Actor: *actor, Note: *note}
	}
	if decision.Actor == "" {
		if decision.Actor, err = c.DefaultActor(); err != nil {
			_, _ = fmt.Fprintf(stderr, "error: resolve operator identity: %v (pass --actor)\n", err)
			return 2
		}
	}
	decided, err := store.DecideChildAdmission(ctx, decision, c.Now())
	switch {
	case errors.Is(err, triggerqueue.ErrTransition):
		_, _ = fmt.Fprintf(stderr, "error: child %s already has a different admission decision\n", decision.ChildID)
		return 1
	case errors.Is(err, sql.ErrNoRows):
		_, _ = fmt.Fprintf(stderr, "error: child %s is not held for human admission\n", decision.ChildID)
		return 1
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "error: record admission decision: %v\n", err)
		return 2
	}
	return writeAdmissionReports(stdout, stderr, []AdmissionReport{NewAdmissionReport(decided)}, *asJSON, "")
}

// NewAdmissionReport decodes bounded pinned evidence for display. Evidence
// that no longer decodes is reported as unavailable rather than trusted.
func NewAdmissionReport(review triggerqueue.ChildAdmissionReview) AdmissionReport {
	report := AdmissionReport{
		ChildID: review.ChildID, Gaggle: review.Identity.Gaggle, ParentRunID: review.Identity.ParentRunID,
		StageOccurrence: review.Identity.StageOccurrence, InvocationKey: review.Identity.InvocationKey,
		State: string(review.State), Reason: review.Reason, RequestedAt: review.RequestedAt,
		Actor: review.Actor, Note: review.Note, Summary: "novelty evidence unavailable",
	}
	if !review.DecidedAt.IsZero() {
		decided := review.DecidedAt
		report.DecidedAt = &decided
	}
	var evidence journal.ChildNovelty
	var summary workflowcluster.Novelty
	if json.Unmarshal(review.Evidence, &evidence) == nil && evidence.Valid() && json.Unmarshal(review.Evidence, &summary) == nil {
		report.Novelty, report.Summary = &evidence, summary.Summary()
	}
	return report
}

func writeAdmissionReports(stdout, stderr io.Writer, reports []AdmissionReport, asJSON bool, empty string) int {
	var err error
	switch {
	case asJSON:
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(reports)
	case len(reports) == 0:
		_, err = fmt.Fprintln(stdout, empty)
	default:
		for _, r := range reports {
			if _, err = fmt.Fprintf(stdout, "%s  %s  parent %s/%s stage %s  requested %s\n  %s\n", r.ChildID, r.State, r.Gaggle, r.ParentRunID, r.StageOccurrence, r.RequestedAt.Format(time.RFC3339), r.Summary); err != nil {
				break
			}
		}
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: write admission report: %v\n", err)
		return 2
	}
	return 0
}
