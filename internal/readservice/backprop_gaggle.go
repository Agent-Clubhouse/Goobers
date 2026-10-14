package readservice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// GaggleBackpropReportSchema identifies the gaggle-level Backprop report
// contract (#7120).
const GaggleBackpropReportSchema = "goobers.dev/backprop/gaggle-report/v1"

// Cross-workflow link bases. Each names the recorded identifier an edge rests
// on; there is no inferred or similarity-based basis.
const (
	GaggleLinkSharedExternalRef = "shared-external-ref"
	GaggleLinkContinuedFrom     = "continued-from-run"
)

const (
	defaultGaggleReportWindow = 7 * 24 * time.Hour
	defaultGaggleReportLimit  = 500
	gaggleReportTopFindings   = 10
)

// GaggleBackpropQuery scopes a gaggle report. Gaggle is required. A zero
// Until is now; a zero Since is seven days before Until. The previous window
// is the same length immediately before Since. Limit caps the active records
// read per window (default 500).
type GaggleBackpropQuery struct {
	Gaggle string
	Since  time.Time
	Until  time.Time
	Limit  int
}

// GaggleBackpropReport is the holistic, read-only view of one gaggle's active
// Backprop records: how credit and blame split across its workflows and
// stages, which findings recur, how each workflow trended against the
// previous window, and the cross-workflow links its runs recorded. Shadow
// records never appear here.
type GaggleBackpropReport struct {
	Schema            string             `json:"schema"`
	Gaggle            string             `json:"gaggle"`
	Window            GaggleReportWindow `json:"window"`
	PreviousWindow    GaggleReportWindow `json:"previousWindow"`
	Runs              int                `json:"runs"`
	Truncated         bool               `json:"truncated,omitempty"`
	PreviousTruncated bool               `json:"previousTruncated,omitempty"`
	// LinkEvidenceUnavailable lists runs in the window whose journal could
	// not be read, so their recorded identifiers (and any links through
	// them) are missing from Links.
	LinkEvidenceUnavailable []string                  `json:"linkEvidenceUnavailable,omitempty"`
	Workflows               []GaggleWorkflowCredit    `json:"workflows"`
	Stages                  []GaggleStageCredit       `json:"stages"`
	RecurringFindings       []GaggleRecurringFinding  `json:"recurringFindings"`
	Trends                  []GaggleWorkflowTrend     `json:"trends"`
	Links                   []GaggleCrossWorkflowLink `json:"links"`
}

// GaggleReportWindow is a half-open [Since, Until) run-start window.
type GaggleReportWindow struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// GaggleWorkflowCredit is one workflow's share of the gaggle's attributed
// outcomes. A complete record with a positive outcome contributes one unit of
// credit, one with a negative outcome one unit of blame, and one with a
// neutral outcome (for example an escalated run) neither; shares are each
// workflow's units over the gaggle's total. Failed counts failed and aborted
// runs, the phases that carry a negative outcome.
type GaggleWorkflowCredit struct {
	Workflow             string  `json:"workflow"`
	Runs                 int     `json:"runs"`
	Failed               int     `json:"failed"`
	Complete             int     `json:"complete"`
	Neutral              int     `json:"neutral"`
	InsufficientEvidence int     `json:"insufficientEvidence"`
	RecordFailures       int     `json:"recordFailures"`
	CreditShare          float64 `json:"creditShare"`
	BlameShare           float64 `json:"blameShare"`
	// DownstreamFailures counts failed runs of other workflows that a
	// recorded link places after one of this workflow's runs.
	DownstreamFailures int `json:"downstreamFailures"`
}

// GaggleStageCredit is one workflow stage's share of the gaggle's credit and
// blame. Each complete record's stage contributions are normalized to one
// unit before they are pooled, so long runs do not outweigh short ones.
type GaggleStageCredit struct {
	Workflow    string  `json:"workflow"`
	Stage       string  `json:"stage"`
	Runs        int     `json:"runs"`
	CreditShare float64 `json:"creditShare"`
	BlameShare  float64 `json:"blameShare"`
}

// GaggleRecurringFinding is a cause class that recurred at one workflow
// stage in at least two runs in the window.
type GaggleRecurringFinding struct {
	Workflow string                   `json:"workflow"`
	Stage    string                   `json:"stage,omitempty"`
	Class    creditgraph.FailureClass `json:"class"`
	Runs     int                      `json:"runs"`
	RunIDs   []string                 `json:"runIds"`
}

// GaggleWorkflowTrend compares one workflow's window against the previous
// window of the same length.
type GaggleWorkflowTrend struct {
	Workflow            string  `json:"workflow"`
	Runs                int     `json:"runs"`
	PreviousRuns        int     `json:"previousRuns"`
	FailureRate         float64 `json:"failureRate"`
	PreviousFailureRate float64 `json:"previousFailureRate"`
	BlameShare          float64 `json:"blameShare"`
	PreviousBlameShare  float64 `json:"previousBlameShare"`
}

// GaggleCrossWorkflowLink is one recorded edge from an earlier run to a later
// run of the gaggle. Basis names the recorded identifier both runs share; an
// edge exists only when the identifier matched exactly.
type GaggleCrossWorkflowLink struct {
	Basis        string           `json:"basis"`
	Identifier   string           `json:"identifier"`
	FromRunID    string           `json:"fromRunId"`
	FromWorkflow string           `json:"fromWorkflow"`
	ToRunID      string           `json:"toRunId"`
	ToWorkflow   string           `json:"toWorkflow"`
	ToPhase      journal.RunPhase `json:"toPhase"`
}

type gaggleReportRun struct {
	runID         string
	workflow      string
	startedAt     time.Time
	phase         journal.RunPhase
	record        creditgraph.RunRecord
	refs          []string
	continuedFrom string
	// linkEvidenceMissing reports that the run's journal could not be read.
	linkEvidenceMissing bool
}

// StoredGaggleBackprop builds the gaggle-level Backprop report from the
// active attribution records of terminal runs in the gaggle. It never writes
// state and never reads shadow records.
func StoredGaggleBackprop(
	ctx context.Context,
	root string,
	reads readmodel.Reader,
	query GaggleBackpropQuery,
	now time.Time,
) (GaggleBackpropReport, error) {
	if strings.TrimSpace(query.Gaggle) == "" {
		return GaggleBackpropReport{}, errors.New("gaggle report requires a gaggle")
	}
	window, previous := gaggleReportWindows(query, now)
	limit := query.Limit
	if limit < 1 {
		limit = defaultGaggleReportLimit
	}
	current, truncated, err := scanGaggleReportRuns(ctx, root, reads, query.Gaggle, window, limit)
	if err != nil {
		return GaggleBackpropReport{}, err
	}
	prior, priorTruncated, err := scanGaggleReportRuns(ctx, root, reads, query.Gaggle, previous, limit)
	if err != nil {
		return GaggleBackpropReport{}, err
	}
	links := gaggleCrossWorkflowLinks(current)
	workflows := gaggleWorkflowCredit(current, links)
	return GaggleBackpropReport{
		Schema: GaggleBackpropReportSchema, Gaggle: query.Gaggle,
		Window: window, PreviousWindow: previous,
		Runs: len(current), Truncated: truncated, PreviousTruncated: priorTruncated,
		LinkEvidenceUnavailable: gaggleLinkEvidenceUnavailable(current),
		Workflows:               workflows,
		Stages:                  gaggleStageCredit(current),
		RecurringFindings:       gaggleRecurringFindings(current),
		Trends:                  gaggleWorkflowTrends(workflows, gaggleWorkflowCredit(prior, nil)),
		Links:                   links,
	}, nil
}

func gaggleReportWindows(query GaggleBackpropQuery, now time.Time) (GaggleReportWindow, GaggleReportWindow) {
	until := query.Until
	if until.IsZero() {
		until = now
	}
	since := query.Since
	if since.IsZero() || !since.Before(until) {
		since = until.Add(-defaultGaggleReportWindow)
	}
	length := until.Sub(since)
	return GaggleReportWindow{Since: since.UTC(), Until: until.UTC()},
		GaggleReportWindow{Since: since.Add(-length).UTC(), Until: since.UTC()}
}

// scanGaggleReportRuns reads the active records in one window. truncated
// reports that the record limit or the scan budget may have cut the window
// short.
func scanGaggleReportRuns(
	ctx context.Context,
	root string,
	reads readmodel.Reader,
	gaggle string,
	window GaggleReportWindow,
	limit int,
) ([]gaggleReportRun, bool, error) {
	var runs []gaggleReportRun
	scanned := 0
	observe := func(_ context.Context, layout instance.Layout, row readmodel.RunRow) (creditgraph.AttributionObservation, bool, error) {
		scanned++
		if row.StartedAt.Before(window.Since) || !row.StartedAt.Before(window.Until) {
			return creditgraph.AttributionObservation{}, false, nil
		}
		run, ok, err := readGaggleReportRun(layout, row)
		if err != nil || !ok {
			return creditgraph.AttributionObservation{}, false, err
		}
		runs = append(runs, run)
		return creditgraph.AttributionObservation{RunID: row.RunID}, true, nil
	}
	query := StoredAttributionQuery{Gaggle: gaggle, Since: window.Since, Until: window.Until}
	if _, err := scanAttributionObservations(ctx, root, reads, query, limit, observe); err != nil {
		return nil, false, err
	}
	truncated := len(runs) >= limit || scanned >= max(attributionListPageSize, limit*faultAuditScanBudgetMultiplier)
	sort.Slice(runs, func(i, j int) bool { return gaggleRunBefore(runs[i], runs[j]) })
	return runs, truncated, nil
}

func readGaggleReportRun(layout instance.Layout, row readmodel.RunRow) (gaggleReportRun, bool, error) {
	runDir, err := layout.FindRunDir(row.RunID)
	if err != nil {
		return gaggleReportRun{}, false, fmt.Errorf("open gaggle report run %q: %w", row.RunID, err)
	}
	record, err := creditgraph.ReadRunRecord(runDir)
	if errors.Is(err, os.ErrNotExist) {
		return gaggleReportRun{}, false, nil
	}
	if err != nil {
		return gaggleReportRun{}, false, fmt.Errorf("read attribution record %q: %w", row.RunID, err)
	}
	run := gaggleReportRun{
		runID: row.RunID, workflow: firstNonEmpty(record.Workflow, row.Workflow),
		startedAt: row.StartedAt, phase: row.Phase, record: record,
	}
	// Without a readable journal the run still counts toward credit, but the
	// report flags it: its recorded identifiers, and so its links, are unknown.
	reader, err := journal.OpenReadOnly(runDir)
	if err != nil {
		run.linkEvidenceMissing = true
		return run, true, nil
	}
	identity, err := reader.Identity()
	if err != nil {
		run.linkEvidenceMissing = true
		return run, true, nil
	}
	run.continuedFrom = identity.ContinuedFromRunID
	events, err := reader.Events()
	if err != nil {
		run.linkEvidenceMissing = true
		return run, true, nil
	}
	seen := map[string]bool{}
	for _, event := range events {
		if !event.IsReferenceTouch() {
			continue
		}
		key := externalRefIdentifier(*event.ExternalRef)
		if key != "" && !seen[key] {
			seen[key] = true
			run.refs = append(run.refs, key)
		}
	}
	return run, true, nil
}

// externalRefIdentifier is the normative (Provider, Kind, ID) identity of a
// recorded external reference. URLs are never compared.
func externalRefIdentifier(ref journal.ExternalRef) string {
	provider, kind, id := strings.TrimSpace(ref.Provider), strings.TrimSpace(ref.Kind), strings.TrimSpace(ref.ID)
	if provider == "" || kind == "" || id == "" {
		return ""
	}
	return provider + "/" + kind + "/" + id
}

func gaggleRunBefore(left, right gaggleReportRun) bool {
	if !left.startedAt.Equal(right.startedAt) {
		return left.startedAt.Before(right.startedAt)
	}
	return left.runID < right.runID
}

// gaggleCrossWorkflowLinks emits an edge only from an identifier both runs
// recorded: the same external reference touched by runs of different
// workflows, or a recorded continuation across workflows. A same-workflow
// continuation is a retry, not a handoff, so it is never an edge. runs must be
// in start order.
func gaggleCrossWorkflowLinks(runs []gaggleReportRun) []GaggleCrossWorkflowLink {
	links := []GaggleCrossWorkflowLink{}
	byID := make(map[string]gaggleReportRun, len(runs))
	byRef := map[string][]gaggleReportRun{}
	for _, run := range runs {
		byID[run.runID] = run
		for _, ref := range run.refs {
			byRef[ref] = append(byRef[ref], run)
		}
	}
	for _, run := range runs {
		if source, ok := byID[run.continuedFrom]; ok && source.workflow != run.workflow {
			links = append(links, newGaggleLink(GaggleLinkContinuedFrom, source.runID, source, run))
		}
	}
	refs := make([]string, 0, len(byRef))
	for ref := range byRef {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		touched := byRef[ref]
		for i := 1; i < len(touched); i++ {
			// Link each run to the nearest earlier run of another workflow,
			// so a chain of handoffs is a chain of edges, not a clique.
			for j := i - 1; j >= 0; j-- {
				if touched[j].workflow != touched[i].workflow {
					links = append(links, newGaggleLink(GaggleLinkSharedExternalRef, ref, touched[j], touched[i]))
					break
				}
			}
		}
	}
	return links
}

func gaggleLinkEvidenceUnavailable(runs []gaggleReportRun) []string {
	var missing []string
	for _, run := range runs {
		if run.linkEvidenceMissing {
			missing = append(missing, run.runID)
		}
	}
	return missing
}

func gaggleRunFailed(phase journal.RunPhase) bool {
	return phase == journal.PhaseFailed || phase == journal.PhaseAborted
}

func newGaggleLink(basis, identifier string, from, to gaggleReportRun) GaggleCrossWorkflowLink {
	return GaggleCrossWorkflowLink{
		Basis: basis, Identifier: identifier,
		FromRunID: from.runID, FromWorkflow: from.workflow,
		ToRunID: to.runID, ToWorkflow: to.workflow, ToPhase: to.phase,
	}
}

func gaggleWorkflowCredit(runs []gaggleReportRun, links []GaggleCrossWorkflowLink) []GaggleWorkflowCredit {
	index := map[string]*GaggleWorkflowCredit{}
	credit, blame := 0.0, 0.0
	for _, run := range runs {
		entry := index[run.workflow]
		if entry == nil {
			entry = &GaggleWorkflowCredit{Workflow: run.workflow}
			index[run.workflow] = entry
		}
		entry.Runs++
		if gaggleRunFailed(run.phase) {
			entry.Failed++
		}
		switch run.record.Status {
		case creditgraph.RecordComplete:
			entry.Complete++
			switch sign := run.record.Attribution.OutcomeSign; {
			case sign > 0:
				entry.CreditShare++
				credit++
			case sign < 0:
				entry.BlameShare++
				blame++
			default:
				entry.Neutral++
			}
		case creditgraph.RecordInsufficientEvidence:
			entry.InsufficientEvidence++
		default:
			entry.RecordFailures++
		}
	}
	downstream := map[string]map[string]bool{}
	for _, link := range links {
		if link.FromWorkflow == link.ToWorkflow || !gaggleRunFailed(link.ToPhase) {
			continue
		}
		if downstream[link.FromWorkflow] == nil {
			downstream[link.FromWorkflow] = map[string]bool{}
		}
		downstream[link.FromWorkflow][link.ToRunID] = true
	}
	workflows := make([]GaggleWorkflowCredit, 0, len(index))
	for _, entry := range index {
		entry.CreditShare = shareOf(entry.CreditShare, credit)
		entry.BlameShare = shareOf(entry.BlameShare, blame)
		entry.DownstreamFailures = len(downstream[entry.Workflow])
		workflows = append(workflows, *entry)
	}
	sort.Slice(workflows, func(i, j int) bool { return workflows[i].Workflow < workflows[j].Workflow })
	return workflows
}

func gaggleStageCredit(runs []gaggleReportRun) []GaggleStageCredit {
	type stageKey struct{ workflow, stage string }
	index := map[stageKey]*GaggleStageCredit{}
	credit, blame := 0.0, 0.0
	for _, run := range runs {
		sign := run.record.Attribution.OutcomeSign
		if run.record.Status != creditgraph.RecordComplete || sign == 0 {
			continue
		}
		shares, total := map[string]float64{}, 0.0
		for _, contribution := range run.record.Attribution.Contributions {
			if contribution.Kind == creditgraph.KindStage && contribution.Stage != "" && contribution.Share > 0 {
				shares[contribution.Stage] += contribution.Share
				total += contribution.Share
			}
		}
		positive := sign > 0
		for stage, share := range shares {
			key := stageKey{run.workflow, stage}
			entry := index[key]
			if entry == nil {
				entry = &GaggleStageCredit{Workflow: run.workflow, Stage: stage}
				index[key] = entry
			}
			entry.Runs++
			if positive {
				entry.CreditShare += share / total
				credit += share / total
			} else {
				entry.BlameShare += share / total
				blame += share / total
			}
		}
	}
	stages := make([]GaggleStageCredit, 0, len(index))
	for _, entry := range index {
		entry.CreditShare = shareOf(entry.CreditShare, credit)
		entry.BlameShare = shareOf(entry.BlameShare, blame)
		stages = append(stages, *entry)
	}
	sort.Slice(stages, func(i, j int) bool {
		if stages[i].Workflow != stages[j].Workflow {
			return stages[i].Workflow < stages[j].Workflow
		}
		return stages[i].Stage < stages[j].Stage
	})
	return stages
}

func gaggleRecurringFindings(runs []gaggleReportRun) []GaggleRecurringFinding {
	type findingKey struct {
		workflow, stage string
		class           creditgraph.FailureClass
	}
	index := map[findingKey]*GaggleRecurringFinding{}
	for _, run := range runs {
		seen := map[findingKey]bool{}
		for _, cause := range run.record.Attribution.Causes {
			key := findingKey{run.workflow, cause.Stage, cause.Class}
			if seen[key] {
				continue
			}
			seen[key] = true
			entry := index[key]
			if entry == nil {
				entry = &GaggleRecurringFinding{Workflow: run.workflow, Stage: cause.Stage, Class: cause.Class}
				index[key] = entry
			}
			entry.Runs++
			entry.RunIDs = append(entry.RunIDs, run.runID)
		}
	}
	findings := []GaggleRecurringFinding{}
	for _, entry := range index {
		if entry.Runs >= 2 {
			findings = append(findings, *entry)
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		left, right := findings[i], findings[j]
		if left.Runs != right.Runs {
			return left.Runs > right.Runs
		}
		if left.Workflow != right.Workflow {
			return left.Workflow < right.Workflow
		}
		if left.Stage != right.Stage {
			return left.Stage < right.Stage
		}
		return left.Class < right.Class
	})
	if len(findings) > gaggleReportTopFindings {
		findings = findings[:gaggleReportTopFindings]
	}
	return findings
}

func gaggleWorkflowTrends(current, previous []GaggleWorkflowCredit) []GaggleWorkflowTrend {
	trends := map[string]*GaggleWorkflowTrend{}
	entry := func(workflow string) *GaggleWorkflowTrend {
		if trends[workflow] == nil {
			trends[workflow] = &GaggleWorkflowTrend{Workflow: workflow}
		}
		return trends[workflow]
	}
	for _, workflow := range current {
		trend := entry(workflow.Workflow)
		trend.Runs, trend.BlameShare = workflow.Runs, workflow.BlameShare
		trend.FailureRate = shareOf(float64(workflow.Failed), float64(workflow.Runs))
	}
	for _, workflow := range previous {
		trend := entry(workflow.Workflow)
		trend.PreviousRuns, trend.PreviousBlameShare = workflow.Runs, workflow.BlameShare
		trend.PreviousFailureRate = shareOf(float64(workflow.Failed), float64(workflow.Runs))
	}
	result := make([]GaggleWorkflowTrend, 0, len(trends))
	for _, trend := range trends {
		result = append(result, *trend)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Workflow < result[j].Workflow })
	return result
}

func shareOf(part, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return roundShare(part / total)
}

func roundShare(value float64) float64 {
	const scale = 10000
	return float64(int64(value*scale+0.5)) / scale
}
