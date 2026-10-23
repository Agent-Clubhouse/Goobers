package acceptance

import (
	"context"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/engine"
)

type fakeStartStore struct {
	identity  engine.StartIdentity
	readErr   error
	fenceErr  error
	confirmed int
	checked   int
	adopted   bool
}

func (s *fakeStartStore) ReadStart(context.Context, string) (engine.StartIdentity, error) {
	return s.identity, s.readErr
}
func (s *fakeStartStore) ConfirmStart(_ context.Context, got engine.StartIdentity, adopt bool) error {
	if got != s.identity {
		return errors.New("identity changed")
	}
	if s.fenceErr != nil {
		return s.fenceErr
	}
	s.confirmed++
	s.adopted = adopt
	return nil
}
func (s *fakeStartStore) CheckStartLease(context.Context) error { s.checked++; return s.fenceErr }

func TestStartAdmissionReadsDurableIdentity(t *testing.T) {
	store := &fakeStartStore{identity: engine.StartIdentity{AcceptanceID: "accepted", RunID: "durable", InstanceID: "instance", OwnerEpoch: 9, StarterID: "leader"}}
	admission := NewStartAdmission(store)
	in, err := admission.AdmitStart(t.Context(), engine.RunInput{AcceptanceID: "accepted", OwnerEpoch: 123, StarterID: "invented"})
	if err != nil || in.RunID != "durable" || in.OwnerEpoch != 9 || in.StarterID != "leader" || in.InstanceID != "instance" {
		t.Fatalf("admitted = %+v, %v", in, err)
	}
	for _, result := range []engine.StartResult{{RunID: in.RunID}, {RunID: in.RunID, AlreadyRunning: true}, {RunID: in.RunID, AlreadyRunning: true, Adopt: true}} {
		if err := admission.ConfirmStart(t.Context(), in, result); err != nil {
			t.Fatal(err)
		}
	}
	if store.confirmed != 3 || !store.adopted {
		t.Fatalf("confirmations = %+v", store)
	}
	if _, err := admission.AdmitStart(t.Context(), engine.RunInput{AcceptanceID: "accepted", RunID: "invented"}); err == nil {
		t.Fatal("accepted invented run ID")
	}
}

func TestStartAdmissionNotMineOnlyRevalidatesLease(t *testing.T) {
	for _, stale := range []bool{false, true} {
		store := &fakeStartStore{}
		if stale {
			store.fenceErr = errors.New("stale lease")
		}
		err := NewStartAdmission(store).ConfirmStart(t.Context(), engine.RunInput{}, engine.StartResult{NotMine: true})
		if store.confirmed != 0 || store.checked != 1 || (err != nil) != stale {
			t.Fatalf("NotMine = %+v, %v", store, err)
		}
	}
}

func TestStartAdmissionRefusesMissingDurableRow(t *testing.T) {
	store := &fakeStartStore{readErr: errors.New("no durable acceptance")}
	_, err := NewStartAdmission(store).AdmitStart(t.Context(), engine.RunInput{AcceptanceID: "trigger-invented", RunID: "invented", OwnerEpoch: 7, StarterID: "invented"})
	if err == nil || store.confirmed != 0 {
		t.Fatalf("missing durable row accepted: %+v, %v", store, err)
	}
}
