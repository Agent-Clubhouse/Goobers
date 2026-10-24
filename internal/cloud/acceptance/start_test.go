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
	pins      RunPins
}

func (s *fakeStartStore) ReadStart(context.Context, string) (engine.StartIdentity, error) {
	return s.identity, s.readErr
}
func (s *fakeStartStore) ConfirmStart(_ context.Context, got engine.StartIdentity, adopt bool, pins RunPins) error {
	if got != s.identity {
		return errors.New("identity changed")
	}
	if s.fenceErr != nil {
		return s.fenceErr
	}
	s.confirmed++
	s.adopted = adopt
	s.pins = pins
	return nil
}
func (s *fakeStartStore) CheckStartLease(context.Context) error { s.checked++; return s.fenceErr }

func TestStartAdmissionReadsDurableIdentity(t *testing.T) {
	store := &fakeStartStore{identity: engine.StartIdentity{AcceptanceID: "accepted", RunID: "durable", InstanceID: "instance", OwnerEpoch: 9, PredecessorEpoch: 9, StarterID: "leader"}}
	admission := (&startAdmission{store: store})
	in, err := admission.AdmitStart(t.Context(), pinnedInput(engine.RunInput{AcceptanceID: "accepted", OwnerEpoch: 123, StarterID: "invented"}))
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
	want, err := RunPinsFor(in)
	if err != nil || store.pins != want || store.pins.ConfigGeneration != in.ConfigGeneration || store.pins.InputDigest == "" {
		t.Fatalf("confirmed pins = %+v, want the started input's %+v (%v)", store.pins, want, err)
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
		err := (&startAdmission{store: store}).ConfirmStart(t.Context(), engine.RunInput{}, engine.StartResult{NotMine: true})
		if store.confirmed != 0 || store.checked != 1 || (err != nil) != stale {
			t.Fatalf("NotMine = %+v, %v", store, err)
		}
	}
}

func TestStartAdmissionRefusesMissingDurableRow(t *testing.T) {
	store := &fakeStartStore{readErr: errors.New("no durable acceptance")}
	_, err := (&startAdmission{store: store}).AdmitStart(t.Context(), engine.RunInput{AcceptanceID: "trigger-invented", RunID: "invented", OwnerEpoch: 7, StarterID: "invented"})
	if err == nil || store.confirmed != 0 {
		t.Fatalf("missing durable row accepted: %+v, %v", store, err)
	}
}

// pinnedInput completes in with the run identity the start fence pins.
func pinnedInput(in engine.RunInput) engine.RunInput {
	in.Gaggle, in.WorkflowName, in.Version = "web", "implement", 3
	in.WorkflowDigest, in.GooberDigest, in.ConfigGeneration = "sha256:workflow", "sha256:goober", "sha256:generation"
	in.BranchNamespace = "goobers"
	return in
}

func TestStartAdmissionNeverRefusesAStartForMissingPins(t *testing.T) {
	store := &fakeStartStore{identity: engine.StartIdentity{AcceptanceID: "accepted", RunID: "durable", InstanceID: "instance", OwnerEpoch: 9, PredecessorEpoch: 9, StarterID: "leader"}}
	admission := &startAdmission{store: store}
	in, err := admission.AdmitStart(t.Context(), engine.RunInput{AcceptanceID: "accepted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.ConfirmStart(t.Context(), in, engine.StartResult{RunID: in.RunID}); err != nil || store.confirmed != 1 {
		t.Fatalf("a start without digests was refused: %v", err)
	}
	if store.pins.ConfigGeneration != "" || store.pins.RunID != "durable" {
		t.Fatalf("pins = %+v, want the run identity with empty pins recorded as empty", store.pins)
	}
}

func TestRunPinsCarryTheStartOwnerInputDigest(t *testing.T) {
	in := pinnedInput(engine.RunInput{AcceptanceID: "accepted", RunID: "run", InstanceID: "instance", OwnerEpoch: 9, StarterID: "leader"})
	first, err := RunPinsFor(in)
	if err != nil {
		t.Fatal(err)
	}
	// A lease successor changes only the owner epoch and starter.
	in.OwnerEpoch, in.StarterID = 10, "successor"
	second, err := RunPinsFor(in)
	if err != nil || first != second {
		t.Fatalf("successor pins %+v differ from %+v (%v)", second, first, err)
	}
	in.ConfigGeneration = "sha256:other-generation"
	third, err := RunPinsFor(in)
	if err != nil || third.InputDigest == first.InputDigest {
		t.Fatalf("a different start input kept the digest: %+v", third)
	}
}
