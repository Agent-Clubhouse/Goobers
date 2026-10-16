package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

const prRetryHelp = "Usage: goobers pr retry --pr <number> --route merge-review|remediation|hold --reason <text>\n" +
	"                        [--gaggle <name>] [--actor <identity>] [--dry-run] [--json] [path]\n\n" +
	"Move a parked or escalated pull request to an explicit lifecycle route\n" +
	"without hand-editing provider labels or durable remediation state:\n" +
	"  merge-review  clear escalation and remediation labels and reset the\n" +
	"                durable retry state so merge-review can elect a lander\n" +
	"  remediation   clear escalation labels, reset the durable retry state\n" +
	"                and apply goobers:needs-remediation\n" +
	"  hold          park the PR for a human (goobers:merge-escalated and\n" +
	"                goobers:needs-human); remediation labels are cleared\n\n" +
	"--dry-run reports the PR's current labels, claim lease, remediation\n" +
	"checkpoint and escalation generation, head/base SHA, and the proposed\n" +
	"transition without mutating anything. Otherwise the transition is applied,\n" +
	"an audit comment naming the operator and reason is recorded on the PR, and\n" +
	"the provider state is re-read to verify the result. Re-running a completed\n" +
	"retry changes nothing. The command refuses a closed, merged, or healthy\n" +
	"PR, a PR whose claim lease is still live, and a merge-review route for a PR\n" +
	"with failing CI or both goobers:needs-remediation and\n" +
	"goobers:blocked-on-sibling.\n" +
	"--reason is required; --actor defaults to the invoking user. The provider\n" +
	"credential is read like any stage's (github:pr:write).\n" +
	"The resulting eligibility is printed and is immediately visible in\n" +
	"goobers status, which reads the same provider labels.\n" +
	"Exit codes: 0 = applied, already applied, or dry-run reported; 1 = refused or\n" +
	"provider error; 2 = usage/IO error.\n"

const (
	prRetryRouteMergeReview  = "merge-review"
	prRetryRouteRemediation  = "remediation"
	prRetryRouteHold         = "hold"
	prRetryAuditCommentTitle = "Operator retry"
	prRetryAuditMarker       = "<!-- pr-retry-audit: "
)

// operatorRetryRecord is the durable audit of the last operator-driven retry,
// embedded in the sticky remediation-state comment and the audit comment.
type operatorRetryRecord struct {
	Route      string    `json:"route"`
	Actor      string    `json:"actor"`
	Reason     string    `json:"reason"`
	RecordedAt time.Time `json:"recordedAt"`
	HeadSHA    string    `json:"headSha"`
	BaseSHA    string    `json:"baseSha"`
}

type prRetryParams struct {
	Number int
	Route  string
	Reason string
	Actor  string
	DryRun bool
}

type prRetryClaim struct {
	Held      bool      `json:"held"`
	RunID     string    `json:"runId,omitempty"`
	Workflow  string    `json:"workflow,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
}

type prRetryCheckpoint struct {
	Found                bool   `json:"found"`
	Cycles               int    `json:"cycles,omitempty"`
	Escalated            bool   `json:"escalated,omitempty"`
	EscalationGeneration int    `json:"escalationGeneration,omitempty"`
	EscalatedHeadSHA     string `json:"escalatedHeadSha,omitempty"`
	EscalatedBaseSHA     string `json:"escalatedBaseSha,omitempty"`
	EscalationOutcome    string `json:"escalationOutcome,omitempty"`
}

type prRetryReport struct {
	PR             int               `json:"pr"`
	Route          string            `json:"route"`
	DryRun         bool              `json:"dryRun"`
	Applied        bool              `json:"applied"`
	NoOp           bool              `json:"noOp"`
	Labels         []string          `json:"labels"`
	Claim          prRetryClaim      `json:"claim"`
	Checkpoint     prRetryCheckpoint `json:"checkpoint"`
	HeadSHA        string            `json:"headSha"`
	BaseSHA        string            `json:"baseSha"`
	AddLabels      []string          `json:"addLabels"`
	RemoveLabels   []string          `json:"removeLabels"`
	ResetState     bool              `json:"resetState"`
	ResultLabels   []string          `json:"resultLabels,omitempty"`
	Eligibility    string            `json:"eligibility"`
	Actor          string            `json:"actor"`
	Reason         string            `json:"reason"`
	StateCommentID string            `json:"-"`
}

type prRetryRefusal struct{ reason string }

func (e prRetryRefusal) Error() string { return e.reason }

func runPRRetry(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("pr retry", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "pr retry")
	gaggle := fs.String("gaggle", "", "gaggle that owns the PR (default $GOOBERS_GAGGLE)")
	number := fs.Int("pr", 0, "pull request number")
	route := fs.String("route", "", "target route: merge-review, remediation, or hold")
	reason := fs.String("reason", "", "required audit reason")
	actor := fs.String("actor", "", "audit identity (default: the invoking user)")
	dryRun := fs.Bool("dry-run", false, "report the proposed transition without mutating anything")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}
	params := prRetryParams{
		Number: *number,
		Route:  strings.TrimSpace(*route),
		Reason: strings.TrimSpace(*reason),
		Actor:  strings.TrimSpace(*actor),
		DryRun: *dryRun,
	}
	if params.Number <= 0 {
		pf(stderr, "error: --pr is required\n")
		return 2
	}
	if !slices.Contains([]string{prRetryRouteMergeReview, prRetryRouteRemediation, prRetryRouteHold}, params.Route) {
		pf(stderr, "error: --route must be merge-review, remediation, or hold\n")
		return 2
	}
	if params.Reason == "" {
		pf(stderr, "error: --reason is required\n")
		return 2
	}
	if params.Actor == "" {
		resolved, err := defaultInterventionActor()
		if err != nil {
			pf(stderr, "error: resolve operator identity: %v (pass --actor)\n", err)
			return 2
		}
		params.Actor = resolved
	}
	if name := strings.TrimSpace(*gaggle); name != "" {
		if err := os.Setenv(executor.GaggleEnvVar, name); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	provider, err := newMergeReviewRemediationProvider(root, repo,
		withStageProviderCapability(capability.GitHubPRWrite),
		withStageProviderMutations("pr"),
	)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	lookup := func() (claimsclient.Entry, bool, error) {
		ledger, err := openStageClaimLedger(layoutFor(root))
		if err != nil {
			return claimsclient.Entry{}, false, fmt.Errorf("open claim ledger: %w", err)
		}
		listing, err := ledger.ListNamespace(claimContext(), providerGaggle(), string(repo.Provider))
		if err != nil {
			return claimsclient.Entry{}, false, fmt.Errorf("read claim ledger: %w", err)
		}
		entry, found := listing.Lookup(pullRequestClaimLedgerKey(providerGaggle(), repo.Provider, params.Number))
		return entry, found, nil
	}
	ctx, cancel := providerCommandContext()
	defer cancel()

	report, err := executePRRetry(ctx, provider, repo, lookup, params, time.Now)
	var refusal prRetryRefusal
	switch {
	case errors.As(err, &refusal):
		pf(stderr, "error: refusing to retry PR #%d: %s\n", params.Number, refusal.reason)
		return 1
	case err != nil:
		pf(stderr, "error: retry PR #%d: %v\n", params.Number, err)
		return 1
	}
	if *asJSON {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		pln(stdout, string(data))
		return 0
	}
	renderPRRetryReport(stdout, report)
	return 0
}

func executePRRetry(
	ctx context.Context,
	provider remediationProvider,
	repo providers.RepositoryRef,
	lookupClaim func() (claimsclient.Entry, bool, error),
	params prRetryParams,
	now func() time.Time,
) (prRetryReport, error) {
	id := strconv.Itoa(params.Number)
	pr, err := provider.GetPullRequest(ctx, repo, id)
	if err != nil {
		return prRetryReport{}, fmt.Errorf("read PR: %w", err)
	}
	comments, err := provider.ListComments(ctx, repo, id)
	if err != nil {
		return prRetryReport{}, fmt.Errorf("list comments: %w", err)
	}
	state, stateCommentID, stateFound := latestRemediationStateForPR(pr.Body, comments)
	claim := prRetryClaim{}
	if lookupClaim != nil {
		entry, found, err := lookupClaim()
		if err != nil {
			return prRetryReport{}, err
		}
		if found && entry.ReleasedAt == nil {
			claim = prRetryClaim{Held: entry.ExpiresAt.After(now()), RunID: entry.RunID, Workflow: entry.Workflow, ExpiresAt: entry.ExpiresAt}
		}
	}

	report := prRetryReport{
		PR:     params.Number,
		Route:  params.Route,
		DryRun: params.DryRun,
		Labels: slices.Clone(pr.Labels),
		Claim:  claim,
		Checkpoint: prRetryCheckpoint{
			Found:                stateFound,
			Cycles:               state.Cycles,
			Escalated:            state.Escalated,
			EscalationGeneration: state.EscalationGeneration,
			EscalatedHeadSHA:     state.EscalatedHeadSHA,
			EscalatedBaseSHA:     state.EscalatedBaseSHA,
			EscalationOutcome:    string(state.EscalationOutcome),
		},
		HeadSHA:        pr.HeadSHA,
		BaseSHA:        pr.BaseSHA,
		Actor:          params.Actor,
		Reason:         params.Reason,
		StateCommentID: stateCommentID,
	}
	add, remove := prRetryLabelDelta(params.Route, pr.Labels)
	report.AddLabels, report.RemoveLabels = add, remove
	report.ResetState = stateCommentID != "" && params.Route != prRetryRouteHold && prRetryStateNeedsReset(state)
	report.Eligibility = prRetryEligibility(prRetryResultLabels(pr.Labels, add, remove))

	if err := prRetryValidate(pr, params, claim); err != nil {
		return report, err
	}
	report.NoOp = len(add) == 0 && len(remove) == 0 && !report.ResetState
	if params.DryRun || report.NoOp {
		return report, nil
	}

	record := operatorRetryRecord{
		Route: params.Route, Actor: params.Actor, Reason: params.Reason,
		RecordedAt: now().UTC(), HeadSHA: pr.HeadSHA, BaseSHA: pr.BaseSHA,
	}
	if stateFound && stateCommentID != "" {
		next := prRetryResetState(state, params.Route)
		next.OperatorRetry = &record
		if err := provider.UpdateComment(ctx, repo, stateCommentID, renderRemediationComment(next)); err != nil {
			return report, fmt.Errorf("reset durable retry state: %w", err)
		}
	}
	if len(add) > 0 || len(remove) > 0 {
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo, ID: id, AddLabels: add, RemoveLabels: remove,
		}); err != nil {
			return report, fmt.Errorf("update labels: %w", err)
		}
	}
	audit, err := prRetryAuditComment(record)
	if err != nil {
		return report, err
	}
	if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{Repository: repo, ID: id, Comment: audit}); err != nil {
		return report, fmt.Errorf("record audit comment: %w", err)
	}

	verified, err := provider.GetPullRequest(ctx, repo, id)
	if err != nil {
		return report, fmt.Errorf("read-after-write: %w", err)
	}
	want := prRetryResultLabels(pr.Labels, add, remove)
	if !sameLabelSet(verified.Labels, want) {
		return report, fmt.Errorf("read-after-write: provider labels are %v, want %v", sortedLabels(verified.Labels), sortedLabels(want))
	}
	report.Applied = true
	report.ResultLabels = slices.Clone(verified.Labels)
	report.Eligibility = prRetryEligibility(verified.Labels)
	return report, nil
}

func prRetryValidate(pr providers.PullRequestSummary, params prRetryParams, claim prRetryClaim) error {
	if pr.Merged || !strings.EqualFold(pr.State, "open") {
		return prRetryRefusal{fmt.Sprintf("PR is %s, not open", pullRequestState(pr))}
	}
	if claim.Held {
		return prRetryRefusal{fmt.Sprintf("claim lease held by run %s until %s", claim.RunID, claim.ExpiresAt.UTC().Format(time.RFC3339))}
	}
	if params.Route == prRetryRouteHold {
		return nil
	}
	if !prRetryParked(pr.Labels) {
		return prRetryRefusal{"PR carries no escalation, remediation, or human-hold label, so there is nothing to retry"}
	}
	if params.Route == prRetryRouteMergeReview && pr.CheckState == providers.CheckStateFailing {
		return prRetryRefusal{"CI is failing, so merge-review cannot land it; use --route remediation"}
	}
	if params.Route == prRetryRouteMergeReview && hasLabelFold(pr.Labels, needsRemediationLabel) && hasLabelFold(pr.Labels, blockedOnSiblingLabel) {
		return prRetryRefusal{"PR is both needs-remediation and blocked-on-sibling; choose --route remediation or hold"}
	}
	return nil
}

func prRetryParked(labels []string) bool {
	for _, label := range []string{providers.LabelMergeEscalated, providers.LabelNeedsHuman, needsRemediationLabel, mergeDemotedLabel} {
		if hasLabelFold(labels, label) {
			return true
		}
	}
	return false
}

func prRetryLabelDelta(route string, labels []string) (add, remove []string) {
	var want, clear []string
	switch route {
	case prRetryRouteMergeReview:
		clear = []string{providers.LabelMergeEscalated, providers.LabelNeedsHuman, needsRemediationLabel, blockedOnSiblingLabel, mergeDemotedLabel}
	case prRetryRouteRemediation:
		want = []string{needsRemediationLabel}
		clear = []string{providers.LabelMergeEscalated, providers.LabelNeedsHuman, blockedOnSiblingLabel, mergeDemotedLabel}
	case prRetryRouteHold:
		want = []string{providers.LabelMergeEscalated, providers.LabelNeedsHuman}
		clear = []string{needsRemediationLabel, blockedOnSiblingLabel}
	}
	for _, label := range want {
		if !hasLabelFold(labels, label) {
			add = append(add, label)
		}
	}
	for _, label := range clear {
		if hasLabelFold(labels, label) {
			remove = append(remove, label)
		}
	}
	return add, remove
}

func prRetryResultLabels(labels, add, remove []string) []string {
	var result []string
	for _, label := range labels {
		if !hasLabelFold(remove, label) {
			result = append(result, label)
		}
	}
	return append(result, add...)
}

// prRetryEligibility names which lifecycle plane will pick the PR up next,
// from the labels the selectors themselves read.
func prRetryEligibility(labels []string) string {
	switch {
	case hasLabelFold(labels, providers.LabelMergeEscalated) || hasLabelFold(labels, providers.LabelNeedsHuman):
		return "held: not eligible for automation until a human clears it"
	case hasLabelFold(labels, needsRemediationLabel):
		return "eligible for pr-remediation"
	default:
		return "eligible for merge-review"
	}
}

func prRetryStateNeedsReset(state remediationState) bool {
	return state.Escalated || state.EscalatedHeadSHA != "" || state.EscalatedBaseSHA != "" ||
		state.LastDiffDigest != "" || state.AttemptsByCause != (remediationAttempts{}) ||
		state.RemediationAttempted || state.InfrastructureFailures > 0 || state.InfrastructureVoided || state.Superseded || state.SupersededCycles > 0
}

func prRetryResetState(state remediationState, route string) remediationState {
	if route == prRetryRouteHold {
		return state
	}
	state.AttemptsByCause = remediationAttempts{}
	state.LastDiffDigest = ""
	state.Escalated = false
	state.EscalatedReason = ""
	state.EscalationOutcome = ""
	state.RemediationAttempted = false
	state.AttemptedCauses = nil
	state.EscalatedHeadSHA = ""
	state.EscalatedBaseSHA = ""
	state.SiblingOverlapContext = ""
	state.StructuralCollisionContext = ""
	state.EscalationCauses = nil
	state.EscalationCausesCleared = false
	state.NoopGuardRepark = false
	state.RunID = ""
	state.ChargedCauses = nil
	state.InfrastructureVoided = false
	state.Superseded = false
	state.SupersededCycles = 0
	state.InfrastructureFailures = 0
	return state
}

func prRetryAuditComment(record operatorRetryRecord) (string, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return "", fmt.Errorf("marshal audit record: %w", err)
	}
	return fmt.Sprintf("**%s** — `%s` routed this PR to `%s`: %s\n\n%s%s -->",
		prRetryAuditCommentTitle, record.Actor, record.Route, record.Reason, prRetryAuditMarker, data), nil
}

func hasLabelFold(labels []string, label string) bool {
	return slices.ContainsFunc(labels, func(have string) bool { return strings.EqualFold(have, label) })
}

func sortedLabels(labels []string) []string {
	out := slices.Clone(labels)
	for i := range out {
		out[i] = strings.ToLower(out[i])
	}
	slices.Sort(out)
	return out
}

func sameLabelSet(a, b []string) bool {
	return slices.Equal(slices.Compact(sortedLabels(a)), slices.Compact(sortedLabels(b)))
}

func renderPRRetryReport(w io.Writer, r prRetryReport) {
	pf(w, "PR #%d  route=%s  head=%s  base=%s\n", r.PR, r.Route, r.HeadSHA, r.BaseSHA)
	pf(w, "  labels:     %s\n", strings.Join(r.Labels, ", "))
	if r.Claim.Held {
		pf(w, "  claim:      held by run %s (%s) until %s\n", r.Claim.RunID, r.Claim.Workflow, r.Claim.ExpiresAt.UTC().Format(time.RFC3339))
	} else {
		pf(w, "  claim:      none live\n")
	}
	if r.Checkpoint.Found {
		pf(w, "  checkpoint: cycles=%d escalated=%t generation=%d escalated-head=%s escalated-base=%s outcome=%s\n",
			r.Checkpoint.Cycles, r.Checkpoint.Escalated, r.Checkpoint.EscalationGeneration,
			r.Checkpoint.EscalatedHeadSHA, r.Checkpoint.EscalatedBaseSHA, r.Checkpoint.EscalationOutcome)
	} else {
		pf(w, "  checkpoint: none recorded\n")
	}
	pf(w, "  transition: add=[%s] remove=[%s] reset-retry-state=%t\n",
		strings.Join(r.AddLabels, ", "), strings.Join(r.RemoveLabels, ", "), r.ResetState)
	switch {
	case r.DryRun:
		pf(w, "dry run: nothing changed\n")
	case r.NoOp:
		pf(w, "already in the requested state: nothing changed\n")
	default:
		pf(w, "applied by %s: %s\n", r.Actor, r.Reason)
		pf(w, "  verified labels: %s\n", strings.Join(r.ResultLabels, ", "))
	}
	pf(w, "  eligibility: %s\n", r.Eligibility)
}
