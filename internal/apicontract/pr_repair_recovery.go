package apicontract

import (
	"strings"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
)

// PR repair recovery routes accept only a retained command identity.
const (
	PRRepairCommandPath          = V1Prefix + "/gaggles/{gaggle}/pr-repairs/{command}"
	PRRepairCheckPath            = PRRepairCommandPath + "/check"
	RoutePRRepairCommand RouteID = "prRepairCommand"
	RoutePRRepairCheck   RouteID = "prRepairCheck"
)

// PRRepairCommand retains original producer receipt and separately attributed checks.
type PRRepairCommand = sessioning.PRRepairCommandView

func prRepairRecoveryRoute(id RouteID) bool {
	return id == RoutePRRepairCommand || id == RoutePRRepairCheck
}
func prRepairRecoveryResponses() map[string]any {
	return map[string]any{"200": jsonResponse("Retained producer acknowledgement and separate exact observation", schemaRef("SessionPRRepairCommand")), "default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope"))}
}
func prRepairRecoveryFixture(at time.Time) PRRepairCommand {
	return PRRepairCommand{ID: "repair-" + strings.Repeat("a", 32), SourceBindingID: "code", State: "observed-applied", RequestDigest: strings.Repeat("b", 64), OperationDigest: strings.Repeat("c", 64), SelectedHeadSHA: strings.Repeat("d", 40), ExpectedHeadSHA: strings.Repeat("d", 40), RunID: strings.Repeat("e", 32), Actor: sessioning.Actor{Issuer: "https://issuer.example", Subject: "producer"}, AcceptedAt: at, AttemptedAt: &at, CompletedAt: &at, Receipt: &sessioning.PRRepairReceipt{OperationDigest: strings.Repeat("c", 64), Outcome: "unknown", MutationAttempted: true}, Observations: []sessioning.PRRepairObservation{{Checker: sessioning.Actor{Issuer: "https://issuer.example", Subject: "checker"}, At: at, Matches: true, CommitID: strings.Repeat("f", 40)}}}
}
