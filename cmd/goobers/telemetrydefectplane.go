package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/telemetryclient"
)

// telemetrydefectplane.go is the daemon side of the defect-nomination
// aggregate read (Goobers#4001, the blocker-1 half of Goobers#3996).
//
// It derives its answer by calling detectCandidateFindingsWithCausalCredit —
// the SAME function `goobers telemetry-query` calls on the local path — and
// then applies the two things the local path does not need and the plane
// cannot do without:
//
//  1. the ADMITTED SET. Only stage-failure-rate, gate-noise,
//     credit-assignment and error-signature are derived at all. The families
//     the ruling did not admit are not filtered out of an answer; they are
//     never asked for.
//  2. REDACTION. Error-signature subjects are normalized before they leave
//     the process (telemetryclient.NormalizeErrorSignature), which is what
//     lets decision 005 R4 keep "raw error signatures stay off the plane"
//     while the highest-yield nomination source still reaches a pod.
//
// Sharing the derivation is deliberate, and it is the same discipline
// daemonRunJournalService applies by calling journalclient.FileCrossRun: two
// implementations of one aggregate would eventually answer two different
// things, and a nomination lane cannot tell which one is right.

// daemonTelemetryDefectAggregateService serves the defect-aggregate route.
type daemonTelemetryDefectAggregateService struct {
	layout instance.Layout
}

func newDaemonTelemetryDefectAggregateService(layout instance.Layout) *daemonTelemetryDefectAggregateService {
	return &daemonTelemetryDefectAggregateService{layout: layout}
}

// planeAggregateAliases maps each admitted plane aggregate onto the CLI's own
// aggregate selector. One table, so the plane and the local path cannot come
// to mean different things by the same name.
var planeAggregateAliases = map[telemetryclient.Aggregate]telemetryAggregate{
	telemetryclient.AggregateStageFailureRate: telemetryAggregateStageFailureRate,
	telemetryclient.AggregateErrorSignature:   telemetryAggregateErrorSignature,
	telemetryclient.AggregateGateNoise:        telemetryAggregateGateNoise,
	telemetryclient.AggregateCreditAssignment: telemetryAggregateCreditAssignment,
}

// DefectAggregates derives the requested admitted families for one gaggle.
func (s *daemonTelemetryDefectAggregateService) DefectAggregates(
	ctx context.Context,
	request httpapi.TelemetryDefectAggregateRequest,
) (telemetryclient.DefectAggregateResponse, error) {
	// Re-validated here rather than trusted from the handler: a service that
	// depends on its transport for containment has no containment at all if a
	// second caller ever appears.
	if err := telemetryclient.ValidateScopeName("gaggle", request.Gaggle); err != nil {
		return telemetryclient.DefectAggregateResponse{}, httpapi.NewInterventionError(
			http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error(), nil)
	}
	if request.Workflow != "" {
		if err := telemetryclient.ValidateScopeName("workflow", request.Workflow); err != nil {
			return telemetryclient.DefectAggregateResponse{}, httpapi.NewInterventionError(
				http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error(), nil)
		}
	}
	if err := telemetryclient.ValidateWindow(request.Since, time.Now().UTC()); err != nil {
		return telemetryclient.DefectAggregateResponse{}, httpapi.NewInterventionError(
			http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error(), nil)
	}

	aggregates := planeAggregateSelectors(request.Aggregates)
	thresholds := planeThresholds(request.Thresholds)

	dbPath := s.layout.TelemetryDB()
	if _, err := os.Stat(dbPath); err != nil {
		if !os.IsNotExist(err) {
			return telemetryclient.DefectAggregateResponse{}, fmt.Errorf("inspect telemetry rollup: %w", err)
		}
		// An instance with telemetry disabled, or one that has not finished a
		// run yet, has no findings. The local path answers that with an empty
		// artifact carrying `no telemetry rollup yet`, and the plane answers
		// the same way rather than with a 503: the note is what makes it
		// visible, and a lane's artifact reads identically either way.
		return telemetryclient.DefectAggregateResponse{
			Findings:            []telemetryclient.Finding{},
			PromotionCandidates: []telemetryclient.PromotionSignal{},
			NoWork:              true,
			Note:                telemetryQueryNoRollupNote,
		}, nil
	}
	db, err := openRollup(s.layout, false)
	if err != nil {
		return telemetryclient.DefectAggregateResponse{}, fmt.Errorf("open telemetry rollup: %w", err)
	}
	defer func() { _ = db.Close() }()

	var creditStore *readmodel.Store
	if _, statErr := os.Stat(s.layout.ReadDB()); statErr == nil {
		creditStore, err = readmodel.Open(s.layout.ReadDB())
		if err != nil {
			return telemetryclient.DefectAggregateResponse{}, fmt.Errorf("open run read model: %w", err)
		}
		defer func() { _ = creditStore.Close() }()
	} else if !os.IsNotExist(statErr) {
		return telemetryclient.DefectAggregateResponse{}, fmt.Errorf("inspect run read model: %w", statErr)
	}

	if err := ctx.Err(); err != nil {
		return telemetryclient.DefectAggregateResponse{}, err
	}
	artifact, err := detectCandidateFindingsWithCausalCredit(
		db, creditStore,
		// Window is not used by the derivation itself — Since is the bound
		// that matters — and the answer carries the caller's own window.
		time.Since(request.Since), request.Since,
		s.layout.Root, request.Gaggle, request.Workflow,
		aggregates, nil, thresholds,
	)
	if err != nil {
		return telemetryclient.DefectAggregateResponse{}, fmt.Errorf("query candidate findings: %w", err)
	}
	return defectAggregateResponse(artifact), nil
}

// planeAggregateSelectors maps the admitted plane names onto the CLI's
// selectors. An empty request means all four admitted families, never "all",
// so a family the ruling did not admit cannot be reached by omission.
func planeAggregateSelectors(requested []telemetryclient.Aggregate) telemetryAggregateValues {
	if len(requested) == 0 {
		requested = telemetryclient.AdmittedAggregates()
	}
	selectors := make(telemetryAggregateValues, 0, len(requested))
	for _, aggregate := range requested {
		selector, admitted := planeAggregateAliases[aggregate]
		if !admitted {
			continue
		}
		selectors = append(selectors, selector)
	}
	return selectors
}

// planeThresholds folds the bounded overrides onto the daemon's defaults. The
// families this plane does not serve keep their defaults: their knobs are not
// expressible in a request, so they cannot be moved by one.
func planeThresholds(overrides telemetryclient.Thresholds) rollup.Thresholds {
	thresholds := rollup.DefaultThresholds()
	if overrides.MinSamples > 0 {
		thresholds.MinSamples = overrides.MinSamples
	}
	if overrides.MaxFailureRate > 0 {
		thresholds.MaxFailureRate = overrides.MaxFailureRate
	}
	if overrides.MinErrorSignatureCount > 0 {
		thresholds.MinErrorSignatureCount = overrides.MinErrorSignatureCount
	}
	if overrides.MinGateEvaluations > 0 {
		thresholds.MinGateEvaluations = overrides.MinGateEvaluations
	}
	if overrides.MaxGateEscalationRate > 0 {
		thresholds.MaxGateEscalationRate = overrides.MaxGateEscalationRate
	}
	if overrides.MaxFlaggedRuns > 0 {
		thresholds.MaxFlaggedRuns = overrides.MaxFlaggedRuns
	}
	if overrides.MinCreditRuns > 0 {
		thresholds.MinCreditRuns = overrides.MinCreditRuns
	}
	if overrides.MinCreditFailureShare > 0 {
		thresholds.MinCreditFailureShare = overrides.MinCreditFailureShare
	}
	if thresholds.MaxFlaggedRuns > telemetryclient.MaxFlaggedRuns {
		thresholds.MaxFlaggedRuns = telemetryclient.MaxFlaggedRuns
	}
	return thresholds
}

// defectAggregateResponse projects the local artifact onto the wire, applying
// the redaction the plane is defined by.
func defectAggregateResponse(artifact candidateFindingsArtifact) telemetryclient.DefectAggregateResponse {
	response := telemetryclient.DefectAggregateResponse{
		Findings:            make([]telemetryclient.Finding, 0, len(artifact.Findings)),
		PromotionCandidates: make([]telemetryclient.PromotionSignal, 0, len(artifact.PromotionCandidates)),
		NoWork:              artifact.NoWork,
		Note:                artifact.Note,
	}
	for _, finding := range artifact.Findings {
		response.Findings = append(response.Findings, wireFinding(redactFindingForPlane(finding)))
	}
	for _, estimate := range artifact.CausalCredit {
		response.CausalCredit = append(response.CausalCredit, telemetryclient.CausalNodeCredit{
			Node:              estimate.Node,
			Effect:            estimate.Effect,
			Lower:             estimate.Lower,
			Upper:             estimate.Upper,
			Identification:    string(estimate.Identification),
			Caveat:            estimate.Caveat,
			TreatedBefore:     estimate.TreatedBefore,
			TreatedAfter:      estimate.TreatedAfter,
			ControlBefore:     estimate.ControlBefore,
			ControlAfter:      estimate.ControlAfter,
			IntervalAvailable: estimate.IntervalAvailable,
			PromotionEligible: estimate.PromotionEligible,
			PromotionSource:   estimate.PromotionSource,
			HasCohortData:     estimate.HasCohortData,
		})
	}
	for _, signal := range artifact.PromotionSignals {
		response.PromotionSignals = append(response.PromotionSignals, wirePromotionSignal(signal))
	}
	for _, signal := range artifact.PromotionCandidates {
		response.PromotionCandidates = append(response.PromotionCandidates, wirePromotionSignal(signal))
	}
	return response
}

// redactFindingForPlane is decision 005 R4's boundary applied to one finding.
//
// Only the error-signature family is rewritten, and only its subject: the
// other three families' subjects are stage, gate and workflow-node names,
// which are the caller's own gaggle's configuration and already reach it
// through every other read it makes. The finding keeps its stable digest in
// Signature so a nominator can dedupe across runs without ever holding the
// raw code.
func redactFindingForPlane(finding rollup.Finding) rollup.Finding {
	if finding.Kind != rollup.FindingErrorSignature {
		return finding
	}
	subject, signature := telemetryclient.NormalizeErrorSignature(finding.Subject)
	finding.Subject = subject
	finding.Signature = signature
	return finding
}

func wireFinding(finding rollup.Finding) telemetryclient.Finding {
	wire := telemetryclient.Finding{
		Kind:              string(finding.Kind),
		Subject:           finding.Subject,
		Metrics:           finding.Metrics,
		Threshold:         finding.Threshold,
		FlaggedRuns:       make([]telemetryclient.JournalPointer, 0, len(finding.FlaggedRuns)),
		Signature:         finding.Signature,
		Classification:    string(finding.Classification),
		RecommendedAction: finding.RecommendedAction,
		ErrorClass:        finding.ErrorClass,
	}
	for _, pointer := range finding.FlaggedRuns {
		wire.FlaggedRuns = append(wire.FlaggedRuns, telemetryclient.JournalPointer{
			RunID: pointer.RunID,
			Seq:   pointer.Seq,
		})
	}
	if finding.NominationGuardrails != nil {
		wire.NominationGuardrails = &telemetryclient.NominationGuardrails{
			DedupeKey:                  finding.NominationGuardrails.DedupeKey,
			RequiresUpstreamCauseCheck: finding.NominationGuardrails.RequiresUpstreamCauseCheck,
			RequiresHumanReview:        finding.NominationGuardrails.RequiresHumanReview,
			GoverningTargetTreatment:   finding.NominationGuardrails.GoverningTargetTreatment,
		}
	}
	return wire
}

func wirePromotionSignal(signal readservice.PromotionSignal) telemetryclient.PromotionSignal {
	return telemetryclient.PromotionSignal{
		Node:              signal.Node,
		Value:             signal.Value,
		Lower:             signal.Lower,
		Upper:             signal.Upper,
		Source:            signal.Source,
		Caveat:            signal.Caveat,
		PromotionEligible: signal.PromotionEligible,
	}
}

// rollupFinding is wireFinding's inverse: the stage-side projection back onto
// the shape `telemetry-query` already emits, so a pod's artifact and a
// daemon's artifact are the same document.
func rollupFinding(wire telemetryclient.Finding) rollup.Finding {
	finding := rollup.Finding{
		Kind:              rollup.FindingKind(wire.Kind),
		Subject:           wire.Subject,
		Metrics:           wire.Metrics,
		Threshold:         wire.Threshold,
		FlaggedRuns:       make([]rollup.JournalPointer, 0, len(wire.FlaggedRuns)),
		Signature:         wire.Signature,
		Classification:    apiv1.LearningClassification(wire.Classification),
		RecommendedAction: wire.RecommendedAction,
		ErrorClass:        wire.ErrorClass,
	}
	if finding.Metrics == nil {
		finding.Metrics = map[string]float64{}
	}
	for _, pointer := range wire.FlaggedRuns {
		finding.FlaggedRuns = append(finding.FlaggedRuns, rollup.JournalPointer{
			RunID: pointer.RunID,
			Seq:   pointer.Seq,
		})
	}
	if wire.NominationGuardrails != nil {
		finding.NominationGuardrails = &rollup.NominationGuardrails{
			DedupeKey:                  wire.NominationGuardrails.DedupeKey,
			RequiresUpstreamCauseCheck: wire.NominationGuardrails.RequiresUpstreamCauseCheck,
			RequiresHumanReview:        wire.NominationGuardrails.RequiresHumanReview,
			GoverningTargetTreatment:   wire.NominationGuardrails.GoverningTargetTreatment,
		}
	}
	return finding
}

func readservicePromotionSignal(wire telemetryclient.PromotionSignal) readservice.PromotionSignal {
	return readservice.PromotionSignal{
		Node:              wire.Node,
		Value:             wire.Value,
		Lower:             wire.Lower,
		Upper:             wire.Upper,
		Source:            wire.Source,
		Caveat:            wire.Caveat,
		PromotionEligible: wire.PromotionEligible,
	}
}

var _ httpapi.TelemetryDefectAggregateService = (*daemonTelemetryDefectAggregateService)(nil)
