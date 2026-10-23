package enginechildplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
)

var _ httpapi.EngineChildPlane = Plane{}

type recordingAdmissions struct {
	admitted  []engine.ChildAdmissionInput
	loaded    []engine.ChildRunBinding
	cancelled []engine.ChildRunBinding
}

func (r *recordingAdmissions) Admit(_ context.Context, in engine.ChildAdmissionInput) (engine.ChildRunBinding, error) {
	r.admitted = append(r.admitted, in)
	return engine.ChildRunBinding{Request: in.Request, ParentWorkflowID: in.ParentWorkflowID}, nil
}
func (r *recordingAdmissions) Load(_ context.Context, b engine.ChildRunBinding) ([]byte, error) {
	r.loaded = append(r.loaded, b)
	return []byte(`{"version":1}`), nil
}
func (r *recordingAdmissions) Cancel(_ context.Context, b engine.ChildRunBinding) error {
	r.cancelled = append(r.cancelled, b)
	return nil
}

func strict(t *testing.T, value any) func(any) error {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return func(target any) error {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		return decoder.Decode(target)
	}
}

func TestNewRequiresAdmissions(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("nil admissions accepted")
	}
}

func TestPlaneDecodesEngineTypesAndDelegates(t *testing.T) {
	backend := &recordingAdmissions{}
	plane, err := New(backend)
	if err != nil {
		t.Fatal(err)
	}
	in := engine.ChildAdmissionInput{ParentWorkflow: "parent", ParentWorkflowID: "parent-wf", ParentTemporalRunID: "trun", Stage: "parent", OriginSeq: 7}
	in.Request.ChildRunID = "child-1"
	out, err := plane.Admit(t.Context(), strict(t, in))
	binding, ok := out.(engine.ChildRunBinding)
	if err != nil || !ok || binding.Request.ChildRunID != "child-1" || len(backend.admitted) != 1 || backend.admitted[0] != in {
		t.Fatalf("admit = %#v %v (backend saw %+v)", out, err, backend.admitted)
	}
	manifest, err := plane.Load(t.Context(), strict(t, binding))
	if err != nil || string(manifest) != `{"version":1}` || len(backend.loaded) != 1 || backend.loaded[0].ParentWorkflowID != "parent-wf" {
		t.Fatalf("load = %q %v", manifest, err)
	}
	if err := plane.Cancel(t.Context(), strict(t, binding)); err != nil || len(backend.cancelled) != 1 {
		t.Fatalf("cancel = %v (%d)", err, len(backend.cancelled))
	}
}

func TestPlaneRejectsUndecodableRequestsBeforeCustody(t *testing.T) {
	backend := &recordingAdmissions{}
	plane := Plane{Admissions: backend}
	unknown := strict(t, map[string]any{"notAField": true})
	if _, err := plane.Admit(t.Context(), unknown); err == nil {
		t.Fatal("unknown admission field accepted")
	}
	if _, err := plane.Load(t.Context(), unknown); err == nil {
		t.Fatal("unknown binding field accepted on load")
	}
	if err := plane.Cancel(t.Context(), unknown); err == nil {
		t.Fatal("unknown binding field accepted on cancel")
	}
	failed := errors.New("decode failed")
	if _, err := plane.Admit(t.Context(), func(any) error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("decode error = %v", err)
	}
	if len(backend.admitted)+len(backend.loaded)+len(backend.cancelled) != 0 {
		t.Fatalf("custody reached by undecodable requests: %+v", backend)
	}
}
