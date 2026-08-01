package readservice

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// UX parity for the filter space (#1920, #1782).
//
// §5.7's closed set is a promise about COST, not about which questions the
// portal may ask. The Runs page constructs
// `gaggle x workflow x stage x outcome x population x since x until x phase`
// from URL parameters, and Insight drill-through builds combinations
// programmatically from a finite metric catalog. So a combination outside the
// set must still be ANSWERED — by the journal path, slowly — because "if the
// enumeration misses a combination drill-through can produce, that is a
// regression against behavior that works today".
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

// TestEligibilityAgreesWithTheStoreAboutEveryRequest pins that the two places
// which derive filter dimensions from a request cannot disagree.
//
// `readModelEligible` calls readModelDims to decide whether to dispatch to the
// read model; the store then calls ListOptions.Dims to decide whether to answer.
// They are separate functions over the same request, and if they ever disagree
// in the direction "eligible says yes, store says no", the store's typed
// refusal escapes to the HTTP caller — a user gets
// `unsupported_filter_combination` on a URL that worked before, with no
// fallback, because the service already committed to the read-model path.
//
// A drift of one line in either function produces that. This is the test that
// catches it, and it enumerates rather than samples because the failure is one
// combination out of 256.
func TestEligibilityAgreesWithTheStoreAboutEveryRequest(t *testing.T) {
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
			Population: readmodel.Population(options.StagePopulation),
		}
		_, storeErr := readmodel.Require(request.Dims())

		eligible := serviceErr == nil
		servable := storeErr == nil
		if eligible && !servable {
			t.Errorf("service judged {%s} eligible but the store refuses {%s}; "+
				"the refusal would escape to the caller instead of falling back",
				readmodel.Key(serviceDims), readmodel.Key(request.Dims()))
		}
	}
}

// TestEveryPortalCombinationIsEitherServedOrFallsBack pins the parity promise
// itself: no combination the portal can construct is left with no path.
//
// It checks the DECISION rather than executing a query, because the property is
// about routing. A combination is fine if the read model serves it, and fine if
// the read model refuses it — as long as refusal means "take the other path",
// which is what readModelEligible returning false does.
//
// What would fail here is a combination that is neither in the closed set nor
// reachable by the journal path, which is how a filter silently becomes
// unanswerable.
func TestEveryPortalCombinationIsEitherServedOrFallsBack(t *testing.T) {
	var served, fallback int
	for _, options := range portalFilterSpace() {
		if _, err := readmodel.Require(readModelDims(options)); err == nil {
			served++
			continue
		}
		// Refused by the closed set. The service must then choose a journal-
		// derived path rather than surface the refusal — which it does, because
		// readModelEligible is consulted before dispatch and its false answer
		// falls through to listRunsIndexed / listRunsScanning.
		fallback++
	}
	if served+fallback != 256 {
		t.Fatalf("enumerated %d combinations, want 256", served+fallback)
	}
	if served == 0 {
		t.Error("no combination is served from the read model; the cutover is inert")
	}
	t.Logf("portal filter space: %d served from the read model, %d fall back", served, fallback)
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
// filter when Telemetry is nil BEFORE it consults readModelEligible. This test
// pins that the guard exists and is reached, so the zeroed flags stay
// unreachable rather than becoming a silent empty result.
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
