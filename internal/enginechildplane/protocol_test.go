package enginechildplane

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
)

var _ httpapi.EngineChildProtocol = Protocol{}

type recordingProtocol struct{ calls int }

func (r *recordingProtocol) Park(context.Context, engine.ChildParkInput) error {
	r.calls++
	return nil
}
func (r *recordingProtocol) Finalize(_ context.Context, in engine.ChildFinalizeInput) (childresult.TerminalReturn, error) {
	r.calls++
	return childresult.TerminalReturn{RunID: in.Binding.Child.RunID}, nil
}
func (r *recordingProtocol) Complete(context.Context, engine.ChildCompletionInput) (apiv1.ContextPointer, error) {
	r.calls++
	return apiv1.ContextPointer{Name: "child-result"}, nil
}

// The protocol adapter decodes engine types strictly and delegates; an
// undecodable request never reaches custody.
func TestProtocolDecodesEngineTypesAndDelegates(t *testing.T) {
	if _, err := NewProtocol(nil); err == nil {
		t.Fatal("nil protocol accepted")
	}
	backend := &recordingProtocol{}
	protocol, err := NewProtocol(backend)
	if err != nil {
		t.Fatal(err)
	}
	var in engine.ChildFinalizeInput
	in.Binding.Child.RunID = "child-1"
	if err := protocol.Park(t.Context(), strict(t, engine.ChildParkInput{})); err != nil {
		t.Fatal(err)
	}
	out, err := protocol.Finalize(t.Context(), strict(t, in))
	if result, ok := out.(childresult.TerminalReturn); err != nil || !ok || result.RunID != "child-1" {
		t.Fatalf("finalize = %#v, %v", out, err)
	}
	if out, err := protocol.Complete(t.Context(), strict(t, engine.ChildCompletionInput{})); err != nil || out.(apiv1.ContextPointer).Name != "child-result" {
		t.Fatalf("complete = %#v, %v", out, err)
	}
	unknown := strict(t, map[string]any{"notAField": true})
	failed := errors.New("decode failed")
	if protocol.Park(t.Context(), unknown) == nil {
		t.Fatal("unknown park field accepted")
	}
	if _, err := protocol.Finalize(t.Context(), unknown); err == nil {
		t.Fatal("unknown finalize field accepted")
	}
	if _, err := protocol.Complete(t.Context(), func(any) error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("decode error = %v", err)
	}
	if backend.calls != 3 {
		t.Fatalf("custody calls = %d, want 3 (undecodable requests must not reach it)", backend.calls)
	}
}
