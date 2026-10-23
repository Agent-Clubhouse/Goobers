package engine

import (
	"context"
	"errors"

	"go.temporal.io/sdk/client"
)

// ErrStartNotMine refuses attachment and all owned-run effects.
var ErrStartNotMine = errors.New("engine: execution is not owned by this starter")

// StartAdmission is the cloud composition boundary. AdmitStart reads durable
// acceptance custody under the live lease fence; it must source the run ID and
// owner from that custody, never from caller-supplied identity strings.
// ConfirmStart revalidates the fence after Temporal and commits mine/adopt
// reconciliation before callers may perform owned-run effects. On NotMine it
// only revalidates leadership; it must not mutate acceptance or run state.
type StartAdmission interface {
	AdmitStart(context.Context, RunInput) (RunInput, error)
	ConfirmStart(context.Context, RunInput, StartResult) error
}

// NewCloudTemporalStarter requires durable admission independently of placement.
func NewCloudTemporalStarter(c client.Client, taskQueue string, admission StartAdmission) *TemporalStarter {
	return &TemporalStarter{client: c, taskQueue: taskQueue, admission: admission, cloud: true}
}

func (s *TemporalStarter) admit(ctx context.Context, in RunInput) (RunInput, error) {
	if s.admission != nil {
		return s.admission.AdmitStart(ctx, in)
	}
	if s.cloud || in.AcceptanceID != "" || in.OwnerEpoch != 0 || in.StarterID != "" {
		return RunInput{}, errors.New("engine: cloud start requires durable acceptance admission")
	}
	return in, nil
}

func (s *TemporalStarter) confirm(ctx context.Context, in RunInput, result StartResult) (StartResult, error) {
	if s.admission != nil {
		if err := s.admission.ConfirmStart(ctx, in, result); err != nil {
			return StartResult{}, err
		}
	}
	return result, nil
}

// StartIdentity is read from durable acceptance custody and the live lease.
// A matching pair of caller-supplied strings is never a StartIdentity proof.
type StartIdentity struct {
	AcceptanceID string
	RunID        string
	InstanceID   string
	StarterID    string
	OwnerEpoch   int64
}
