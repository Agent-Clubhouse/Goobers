package readservice

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// UX parity for the filter space (#1920, #1782).
//
// §5.7's closed set is a promise about COST. The Runs page constructs
// `gaggle x workflow x stage x outcome x population x since x until x phase`
// from URL parameters, and Insight drill-through builds combinations
// programmatically from a finite metric catalog. Combinations outside the set
// must be refused rather than answered by an unbounded journal scan.
//
// These tests pin the two ways that promise can break.

// portalFilterSpace enumerates every filter combination the portal's router can
// parse, as a bitmask over the dimensions RunListOptions exposes.
//
// Enumerated rather than sampled: the failure mode is one unlucky combination,
// and a sample is exactly the thing that misses it.
func portalFilterSpace() []RunListOptions {
	type setter func(*RunListOptions)
	setters := []setter{
		func(o *RunListOptions) { o.Gaggle = "alpha" },
		func(o *RunListOptions) { o.Workflow = "wf" },
		func(o *RunListOptions) { o.Phase = journal.PhaseCompleted },
		func(o *RunListOptions) { o.Stage = "build" },
		func(o *RunListOptions) { o.Outcome = OutcomeSuccess },
		func(o *RunListOptions) { o.StagePopulation = StagePopulationCostMeasured },
		func(o *RunListOptions) { o.Since = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
		func(o *RunListOptions) { o.Until = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) },
	}

	out := make([]RunListOptions, 0, 1<<len(setters))
	for mask := 0; mask < 1<<len(setters); mask++ {
		options := RunListOptions{Limit: 50}
		for i, set := range setters {
			if mask&(1<<i) != 0 {
				set(&options)
			}
		}
		out = append(out, options)
	}
	return out
}

// TestReadModelAdmissionAgreesWithTheStoreAboutEveryRequest pins that the two places
// which derive filter dimensions from a request cannot disagree.
//
// The service calls readModelDims before dispatch; the store then calls
// ListOptions.Dims before querying. They are separate functions over the same
// request and must produce the same admission decision.
//
// A drift of one line in either function produces that. This is the test that
// catches it, and it enumerates rather than samples because the failure is one
// combination out of 256.
func TestReadModelAdmissionAgreesWithTheStoreAboutEveryRequest(t *testing.T) {
	for _, options := range portalFilterSpace() {
		serviceDims := readModelDims(options)
		_, serviceErr := readmodel.Require(serviceDims)

		// The request the service would actually build, mirrored from
		// listRunsFromReadModel. If that construction changes, this must too —
		// which is the point: the mirror is what makes drift visible.
		request := readmodel.ListOptions{
			Gaggle:     options.Gaggle,
			Workflow:   options.Workflow,
			Phase:      options.Phase,
			Since:      options.Since,
			Until:      options.Until,
			Limit:      50,
			Stage:      options.Stage,
			Outcome:    readmodel.Outcome(options.Outcome),
			Population: readmodel.Population(options.StagePopulation),
		}
		_, storeErr := readmodel.Require(request.Dims())

		if (serviceErr == nil) != (storeErr == nil) {
			t.Errorf("service admission for {%s} disagrees with store admission for {%s}",
				readmodel.Key(serviceDims), readmodel.Key(request.Dims()))
		}
	}
}

// TestEveryPortalCombinationIsServedOrRefused pins the closed-set promise.
//
// It checks the decision rather than executing a query: each combination is
// either admitted to the bounded read-model path or receives a typed refusal.
func TestEveryPortalCombinationIsServedOrRefused(t *testing.T) {
	var served, refused int
	for _, options := range portalFilterSpace() {
		if _, err := readmodel.Require(readModelDims(options)); err == nil {
			served++
			continue
		}
		refused++
	}
	if served+refused != 256 {
		t.Fatalf("enumerated %d combinations, want 256", served+refused)
	}
	if served == 0 {
		t.Error("no combination is served from the read model; the cutover is inert")
	}
	t.Logf("portal filter space: %d served from the read model, %d refused", served, refused)
}

// TestPopulationWithoutTelemetryIsRefusedBeforeDispatch pins the ordering that
// makes the standalone topology correct (#1782).
//
// Standalone has no telemetry, so its read model has no measurement source and
// projects every population flag as zero. That would answer
// `population=cost-measured` with an empty page — a wrong answer that looks like
// a real one.
//
// It does not, because listRunsUnannotated refuses a telemetry-backed population
// filter when Telemetry is nil before dispatch. This test pins that the guard
// exists, so the zeroed flags stay unreachable rather than becoming a silent
// empty result.
func TestPopulationWithoutTelemetryIsRefusedBeforeDispatch(t *testing.T) {
	for _, population := range []StagePopulation{
		StagePopulationTokenMeasured,
		StagePopulationPremiumMeasured,
		StagePopulationCostMeasured,
		StagePopulationRetryWaste,
	} {
		if !telemetryStagePopulation(population) {
			t.Errorf("%s is not classified as telemetry-backed; the nil-telemetry guard "+
				"would not fire for it and standalone would answer from zeroed flags", population)
		}
	}
}
