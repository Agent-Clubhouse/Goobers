package startfence

import (
	"context"
	"reflect"
	"testing"

	"github.com/goobers/goobers/internal/engine"
)

type annotationRecoveryAdmission struct {
	testStartAdmission
	confirmed engine.RunInput
}

func (a *annotationRecoveryAdmission) ConfirmStart(_ context.Context, in engine.RunInput, _ engine.StartResult) error {
	a.confirmed = in
	return nil
}

func TestAnnotatedPredecessorStartRemainsAdoptable(t *testing.T) {
	in := cloudStartInput(7)
	starter := existingStart(t, StartOwnerMemo{AcceptanceID: in.AcceptanceID, OwnerEpoch: 6, StarterID: "predecessor"})
	legacyDigest, err := InputDigest(in)
	if err != nil {
		t.Fatal(err)
	}
	// The successor resolves the same generation after the annotation fix.
	in.Annotations = map[string]string{"goobers.dev/allow-preview-features": "true"}
	admission := &annotationRecoveryAdmission{}
	starter.admission = admission
	result, err := starter.Start(t.Context(), in)
	if err != nil || !result.Adopt || result.NotMine {
		t.Fatalf("same pinned predecessor is not adoptable: %+v, %v", result, err)
	}
	confirmedDigest, err := InputDigest(admission.confirmed)
	if err != nil || confirmedDigest != legacyDigest || admission.confirmed.Annotations != nil {
		t.Fatalf("confirmed a different input: %+v, %v", admission.confirmed, err)
	}
	if in.Annotations["goobers.dev/allow-preview-features"] != "true" {
		t.Fatal("classification mutated the caller's annotations")
	}
}

func TestAnnotationRecoveryDoesNotAdoptADifferentStart(t *testing.T) {
	for name, change := range map[string]func(*engine.RunInput, *StartOwnerMemo){
		"generation":       func(in *engine.RunInput, _ *StartOwnerMemo) { in.ConfigGeneration = "other" },
		"workflow pin":     func(in *engine.RunInput, _ *StartOwnerMemo) { in.WorkflowDigest = "other" },
		"goober pin":       func(in *engine.RunInput, _ *StartOwnerMemo) { in.GooberDigest = "other" },
		"definition":       func(in *engine.RunInput, _ *StartOwnerMemo) { in.Spec.Start = "other" },
		"instance":         func(in *engine.RunInput, _ *StartOwnerMemo) { in.InstanceID = "other" },
		"acceptance":       func(_ *engine.RunInput, owner *StartOwnerMemo) { owner.AcceptanceID = "other" },
		"missing owner":    func(_ *engine.RunInput, owner *StartOwnerMemo) { owner.StarterID = "" },
		"unowned":          func(_ *engine.RunInput, owner *StartOwnerMemo) { owner.OwnerEpoch = 0 },
		"same epoch":       func(_ *engine.RunInput, owner *StartOwnerMemo) { owner.OwnerEpoch = 7 },
		"future epoch":     func(_ *engine.RunInput, owner *StartOwnerMemo) { owner.OwnerEpoch = 8 },
		"child permission": func(in *engine.RunInput, _ *StartOwnerMemo) { in.ChildWorkflowsEnabled = true },
	} {
		t.Run(name, func(t *testing.T) {
			legacy := cloudStartInput(7)
			legacy.InstanceID, legacy.ConfigGeneration = "instance", "generation"
			legacy.WorkflowDigest, legacy.GooberDigest = "workflow", "goober"
			in := legacy
			in.Annotations = map[string]string{"goobers.dev/allow-preview-features": "true"}
			owner := StartOwnerMemo{AcceptanceID: in.AcceptanceID, OwnerEpoch: 6, StarterID: "predecessor"}
			change(&in, &owner)
			starter := existingStartWithInput(t, legacy, owner)
			result, err := starter.Start(t.Context(), in)
			if err != nil || !result.NotMine || result.Adopt {
				t.Fatalf("different start adopted: %+v, %v", result, err)
			}
		})
	}
}

func TestAnnotatedPredecessorKeepsItsExactInput(t *testing.T) {
	started := cloudStartInput(7)
	started.Annotations = map[string]string{"example.com/policy": "original"}
	for _, changed := range []bool{false, true} {
		in := started
		if changed {
			in.Annotations = map[string]string{"example.com/policy": "different"}
		}
		starter := existingStartWithInput(t, started, StartOwnerMemo{AcceptanceID: in.AcceptanceID, OwnerEpoch: 6, StarterID: "predecessor"})
		admission := &annotationRecoveryAdmission{}
		starter.admission = admission
		result, err := starter.Start(t.Context(), in)
		if err != nil || result.NotMine != changed || result.Adopt == changed {
			t.Fatalf("changed=%v result=%+v, %v", changed, result, err)
		}
		if !changed && !reflect.DeepEqual(admission.confirmed.Annotations, started.Annotations) {
			t.Fatal("annotated start lost its annotations")
		}
	}
}
