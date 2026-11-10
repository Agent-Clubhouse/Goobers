package runsemantics

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry"
)

// NoWorkUnsubstantiatedCode is the terminal cause of a run whose no-work claim
// the runner refused (#2736). A no-work claim short-circuits the run straight
// to completed, so an accepted one is indistinguishable — in the run list, in
// metrics, in every health number an operator looks at — from a healthy empty
// tick. That is the truth when the stage looked and there was genuinely
// nothing to act on; it is silent data loss when the stage found nothing
// because the evidence never reached it. The runner already holds, in the
// journal, what every upstream stage produced, so it looks before accepting.
// Classified in internal/telemetry so the refusal is reportable in operator
// health numbers instead of counting as an unknown failure.
const NoWorkUnsubstantiatedCode = telemetry.ErrCodeNoWorkUnsubstantiated

// StageProduction is what one upstream stage delivered to the run before a
// downstream stage claimed no-work.
type StageProduction struct {
	Stage string
	// Delivered reports that the stage handed this run something a downstream
	// stage could act on — an artifact or an output.
	Delivered bool
}

// DeliveredEvidence reports whether a finished stage's result carried anything
// a downstream stage could act on.
func DeliveredEvidence(result apiv1.ResultEnvelope) bool {
	return len(result.Artifacts) > 0 || len(result.Outputs) > 0
}

// BarrenUpstream names the upstream stages that delivered nothing, and only
// when NONE of them delivered anything: one producing upstream stage is enough
// to make a no-work claim the honest "looked and found nothing", while upstream
// stages that all journaled nothing leave the claim with no basis at all — a
// fan-in stage in that position has no evidence to form any verdict from. Nil
// when the claim is substantiated, and nil when there was no upstream to check.
func BarrenUpstream(upstream []StageProduction) []string {
	if len(upstream) == 0 {
		return nil
	}
	barren := make([]string, 0, len(upstream))
	for _, produced := range upstream {
		if produced.Delivered {
			return nil
		}
		barren = append(barren, produced.Stage)
	}
	return barren
}

// NoWorkUnsubstantiatedMessage is the operator-facing cause both runners record
// for a refused no-work claim, naming the upstream stages that delivered
// nothing so the missing evidence is diagnosable from the terminal row alone.
func NoWorkUnsubstantiatedMessage(stage string, barren []string) string {
	return fmt.Sprintf(
		"stage %q reported no-work, but no upstream stage delivered evidence to act on: %s produced no artifacts and no outputs",
		stage, strings.Join(barren, ", "),
	)
}
