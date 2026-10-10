package readservice

import (
	"context"
	"fmt"
	"sort"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// ShadowBackpropReportSchema identifies the shadow comparison report contract.
const ShadowBackpropReportSchema = "goobers.dev/backprop/shadow-report/v1"

// Shadow run sources.
const (
	ShadowSourceWorkflow       = "workflow"
	ShadowSourceGaggleOverride = "gaggle-override"
)

// ShadowBackpropReport compares what an active fault-audit pass would have
// filed and recommended from shadow attribution with what actually happened.
// It is an isolated, explicitly requested surface: building it never applies
// or records report cooldown, verification baselines, or filing state, and no
// gate, filing pass, or default portal view reads it.
type ShadowBackpropReport struct {
	Schema      string                       `json:"schema"`
	Runs        []ShadowBackpropRun          `json:"runs"`
	WouldFile   creditgraph.FaultAuditReport `json:"wouldFile"`
	Comparisons []ShadowFindingComparison    `json:"comparisons"`
}

// ShadowBackpropRun is one terminal run observed in shadow mode.
type ShadowBackpropRun struct {
	RunID    string                   `json:"runId"`
	Gaggle   string                   `json:"gaggle"`
	Workflow string                   `json:"workflow"`
	Source   string                   `json:"source"`
	Phase    journal.RunPhase         `json:"phase"`
	Status   creditgraph.RecordStatus `json:"status"`
	Causes   int                      `json:"causes"`
}

// ShadowFindingComparison sets one would-be finding beside the actual
// outcome of its runs and whether the active filing pass reported it.
type ShadowFindingComparison struct {
	FindingID         string                   `json:"findingId"`
	Domain            creditgraph.FaultDomain  `json:"domain"`
	RecommendedOwner  string                   `json:"recommendedOwner"`
	RecommendedAction string                   `json:"recommendedAction"`
	RunIDs            []string                 `json:"runIds"`
	ActualPhases      map[journal.RunPhase]int `json:"actualPhases"`
	ActuallyFiled     bool                     `json:"actuallyFiled"`
	ActuallyFiledAt   *time.Time               `json:"actuallyFiledAt,omitempty"`
}

// StoredShadowBackprop builds the shadow comparison report for terminal runs
// in scope. overrides maps gaggle name to its Backprop override; a run whose
// workflow declares no mode of its own is observed in shadow mode when its
// gaggle's override is shadow.
func StoredShadowBackprop(
	ctx context.Context,
	root string,
	reads readmodel.Reader,
	query StoredAttributionQuery,
	overrides map[string]*apiv1.GaggleBackprop,
	config creditgraph.FaultAuditConfig,
) (ShadowBackpropReport, error) {
	maxObservations := config.MaxObservations
	if maxObservations < 1 {
		maxObservations = 500
	}
	report := ShadowBackpropReport{Schema: ShadowBackpropReportSchema, Runs: []ShadowBackpropRun{}, Comparisons: []ShadowFindingComparison{}}
	phases := map[string]journal.RunPhase{}
	observe := func(_ context.Context, layout instance.Layout, row readmodel.RunRow) (creditgraph.AttributionObservation, bool, error) {
		runDir, err := layout.FindRunDir(row.RunID)
		if err != nil {
			return creditgraph.AttributionObservation{}, false, fmt.Errorf("open shadow run %q: %w", row.RunID, err)
		}
		shadow, ok, err := creditgraph.ObserveShadowRun(runDir, overrides[row.Gaggle])
		if err != nil {
			return creditgraph.AttributionObservation{}, false, fmt.Errorf("observe shadow run %q: %w", row.RunID, err)
		}
		if !ok {
			return creditgraph.AttributionObservation{}, false, nil
		}
		source := ShadowSourceWorkflow
		if shadow.ViaOverride {
			source = ShadowSourceGaggleOverride
		}
		report.Runs = append(report.Runs, ShadowBackpropRun{
			RunID: row.RunID, Gaggle: row.Gaggle, Workflow: row.Workflow, Source: source,
			Phase: row.Phase, Status: shadow.Record.Status, Causes: len(shadow.Record.Attribution.Causes),
		})
		phases[row.RunID] = row.Phase
		return attributionObservationFromRecord(layout, row, runDir, shadow.Record)
	}
	observations, err := scanAttributionObservations(ctx, root, reads, query, maxObservations, observe)
	if err != nil {
		return ShadowBackpropReport{}, err
	}
	if config.Since.IsZero() {
		config.Since = query.Since
	}
	if config.Until.IsZero() {
		config.Until = query.Until
	}
	if config.Now.IsZero() {
		config.Now = time.Now().UTC()
	}
	config.PreviousReports, config.FixesAppliedAt, config.BaselineObservations = nil, nil, nil
	report.WouldFile = creditgraph.AuditFaultDomains(observations, config)
	report.WouldFile.Mode = "shadow"
	if len(observations) == 0 {
		return report, nil
	}
	state, err := readFaultAuditState(root)
	if err != nil {
		return ShadowBackpropReport{}, err
	}
	filed := previousReportsForScope(state.PreviousReports, query)
	for _, findings := range [][]creditgraph.FaultFinding{
		report.WouldFile.ProductFindings, report.WouldFile.ExternalFindings,
		report.WouldFile.WorkflowFindings, report.WouldFile.UnknownFindings,
	} {
		for _, finding := range findings {
			comparison := ShadowFindingComparison{
				FindingID: finding.ID, Domain: finding.Domain,
				RecommendedOwner: finding.RecommendedOwner, RecommendedAction: finding.RecommendedAction,
				RunIDs: append([]string(nil), finding.RunIDs...), ActualPhases: map[journal.RunPhase]int{},
			}
			for _, runID := range finding.RunIDs {
				comparison.ActualPhases[phases[runID]]++
			}
			if at, ok := filed[finding.ID]; ok {
				comparison.ActuallyFiled = true
				comparison.ActuallyFiledAt = &at
			}
			report.Comparisons = append(report.Comparisons, comparison)
		}
	}
	sort.Slice(report.Comparisons, func(i, j int) bool { return report.Comparisons[i].FindingID < report.Comparisons[j].FindingID })
	return report, nil
}

// GaggleBackpropOverrides indexes each gaggle's Backprop override by name.
func GaggleBackpropOverrides(gaggles []apiv1.Gaggle) map[string]*apiv1.GaggleBackprop {
	overrides := make(map[string]*apiv1.GaggleBackprop, len(gaggles))
	for i := range gaggles {
		if gaggles[i].Spec.Backprop != nil {
			overrides[gaggles[i].Name] = gaggles[i].Spec.Backprop
		}
	}
	return overrides
}
