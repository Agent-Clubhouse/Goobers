package readservice

import (
	"errors"
	"os"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

// RootIdentity describes durable root identity, independently of read-model
// freshness and of the identity declared by the configuration manifest.
type RootIdentity struct {
	ID                 string     `json:"id,omitempty"`
	IdentityProblem    string     `json:"identityProblem,omitempty"`
	DecommissionedAt   *time.Time `json:"decommissionedAt,omitempty"`
	DecommissionReason string     `json:"decommissionReason,omitempty"`
	LifecycleProblem   string     `json:"lifecycleProblem,omitempty"`
}

func inspectRootIdentity(root string) *RootIdentity {
	result := &RootIdentity{}
	id, err := instance.ReadRootIdentity(root)
	switch {
	case err == nil:
		result.ID = id
	case errors.Is(err, os.ErrNotExist):
		result.IdentityProblem = "Legacy root has no durable identity; inspection does not adopt it."
	default:
		result.IdentityProblem = "Root identity is invalid or unreadable."
	}
	marker, err := instance.ReadRootDecommission(root)
	if err == nil {
		result.DecommissionedAt, result.DecommissionReason = &marker.At, marker.Reason
	} else if !errors.Is(err, os.ErrNotExist) {
		result.LifecycleProblem = "Historical-root marker is invalid or unreadable; root authority is unknown."
	}
	return result
}
