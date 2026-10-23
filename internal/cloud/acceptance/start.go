package acceptance

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/engine"
)

// StartStore is acceptance custody bound to a live controller lease. Each
// method must fence against that lease; ReadStart sources immutable run_id
// from the persisted acceptance, and ConfirmStart performs fenced adoption.
type StartStore interface {
	ReadStart(context.Context, string) (engine.StartIdentity, error)
	ConfirmStart(context.Context, engine.StartIdentity, bool) error
	CheckStartLease(context.Context) error
}

type startAdmission struct{ store StartStore }

// NewStartAdmission wires durable acceptance into the cloud Temporal starter.
func NewStartAdmission(store StartStore) engine.StartAdmission {
	return &startAdmission{store: store}
}

func (a *startAdmission) AdmitStart(ctx context.Context, in engine.RunInput) (engine.RunInput, error) {
	if a.store == nil || in.AcceptanceID == "" {
		return engine.RunInput{}, errors.New("cloud start requires acceptance custody")
	}
	identity, err := a.store.ReadStart(ctx, in.AcceptanceID)
	if err != nil {
		return engine.RunInput{}, err
	}
	if identity.AcceptanceID != in.AcceptanceID || identity.RunID == "" || identity.InstanceID == "" || identity.OwnerEpoch < 1 || identity.StarterID == "" {
		return engine.RunInput{}, errors.New("cloud start acceptance identity is incomplete")
	}
	if (in.RunID != "" && in.RunID != identity.RunID) || (in.InstanceID != "" && in.InstanceID != identity.InstanceID) {
		return engine.RunInput{}, errors.New("cloud start identity differs from durable acceptance")
	}
	in.RunID, in.InstanceID = identity.RunID, identity.InstanceID
	in.OwnerEpoch, in.StarterID = identity.OwnerEpoch, identity.StarterID
	return in, nil
}

func (a *startAdmission) ConfirmStart(ctx context.Context, in engine.RunInput, result engine.StartResult) error {
	if result.NotMine {
		return a.store.CheckStartLease(ctx)
	}
	if result.RunID != in.RunID {
		return errors.New("cloud start returned a different workflow ID")
	}
	return a.store.ConfirmStart(ctx, engine.StartIdentity{
		AcceptanceID: in.AcceptanceID, RunID: in.RunID, InstanceID: in.InstanceID,
		OwnerEpoch: in.OwnerEpoch, StarterID: in.StarterID,
	}, result.Adopt)
}
