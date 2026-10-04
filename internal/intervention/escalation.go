package intervention

import (
	"context"
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/httpapi"
)

// EscalationResolver maps the HITL plane's resolution vocabulary
// (approve/deny/redirect) onto the intervention service's existing escalated-
// run operations, so the plane reuses the resume/override machinery — and its
// journaling — rather than forking it.
type EscalationResolver struct {
	interventions *Service
}

// NewEscalationResolver adapts interventions to the HITL plane.
func NewEscalationResolver(interventions *Service) *EscalationResolver {
	return &EscalationResolver{interventions: interventions}
}

// AcceptResolve applies one escalation resolution.
func (a *EscalationResolver) AcceptResolve(admission, execution context.Context, input httpapi.EscalationResolutionRequest) (httpapi.InterventionResult, error) {
	request := httpapi.InterventionRequest{
		RunID:          input.RunID,
		Stage:          input.Gate,
		IdempotencyKey: input.IdempotencyKey,
		Actor:          input.Actor,
		Decision:       input.Decision,
		Rationale:      input.Rationale,
	}
	switch input.Resolution {
	case httpapi.EscalationResolutionApprove:
		if strings.TrimSpace(input.Gate) == "" {
			return httpapi.InterventionResult{}, interventionBadRequest("gate_required", "approve requires the escalated gate")
		}
		return a.interventions.AcceptApprove(admission, execution, request)
	case httpapi.EscalationResolutionRedirect:
		if strings.TrimSpace(input.Gate) == "" {
			return httpapi.InterventionResult{}, interventionBadRequest("gate_required", "redirect requires the escalated gate")
		}
		if strings.TrimSpace(input.Decision) == "" {
			return httpapi.InterventionResult{}, interventionBadRequest("decision_required", "redirect requires a branch decision")
		}
		return a.interventions.AcceptOverride(admission, execution, request)
	case httpapi.EscalationResolutionDeny:
		return a.interventions.AcceptDenyEscalation(admission, execution, request)
	default:
		return httpapi.InterventionResult{}, interventionBadRequest(
			"invalid_resolution",
			fmt.Sprintf("resolution %q must be approve, deny, or redirect", input.Resolution),
		)
	}
}
