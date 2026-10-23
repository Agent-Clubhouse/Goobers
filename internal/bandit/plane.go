package bandit

import (
	"context"
	"errors"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ObservationRequest has a closed operation vocabulary. It carries no query,
// path, database selector or caller-supplied owner epoch.
type ObservationRequest struct {
	Operation string             `json:"operation"`
	RunID     string             `json:"runId"`
	Stage     string             `json:"stage"`
	Record    *ObservationRecord `json:"record,omitempty"`
}

// ObservationResponse returns samples only for the read operation.
type ObservationResponse struct {
	Observations []Observation `json:"observations,omitempty"`
}

// ObservationPlane is the engine's transport-neutral observation seam.
type ObservationPlane interface {
	Observe(context.Context, ObservationRequest) (ObservationResponse, error)
}

// Validate refuses unknown operations and any mismatch in the attempt identity.
func (r ObservationRequest) Validate() error {
	if !apiv1.ValidRunID(r.RunID) || strings.TrimSpace(r.Stage) == "" || len(r.Stage) > 256 {
		return errors.New("bandit: valid run and stage are required")
	}
	switch r.Operation {
	case "read":
		if r.Record != nil {
			return errors.New("bandit: read must not contain an outcome")
		}
	case "record":
		if r.Record == nil || r.Record.Key.RunID != r.RunID || r.Record.Key.Stage != r.Stage {
			return errors.New("bandit: outcome identity must match the request")
		}
		if _, _, err := EncodeRecord(*r.Record); err != nil {
			return err
		}
	default:
		return errors.New("bandit: operation must be read or record")
	}
	return nil
}
