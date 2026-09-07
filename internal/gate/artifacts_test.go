package gate

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
)

type testArtifactReader struct {
	closed   bool
	closeErr error
}

func (*testArtifactReader) ReadArtifact(context.Context, apiv1.ArtifactPointer, int64) ([]byte, error) {
	return nil, artifactset.ErrInvalid
}
func (r *testArtifactReader) Close() error { r.closed = true; return r.closeErr }

func TestArtifactChecksAreSeparateAndRunScoped(t *testing.T) {
	r := &testArtifactReader{}
	e := NewAutomatedEvaluator()
	opened := 0
	e.OpenArtifacts = func(_ context.Context, run, gaggle string) (ArtifactReader, error) {
		opened++
		if run != "run-one" || gaggle != "gaggle-one" {
			t.Fatalf("wrong journal identity: %s/%s", gaggle, run)
		}
		return r, nil
	}
	pointer := apiv1.ArtifactPointer{Path: "artifacts/index", Digest: apiv1.Digest(nil)}
	env := apiv1.InvocationEnvelope{RunID: "run-one", Gaggle: "gaggle-one", Inputs: map[string]interface{}{InputKeyStatus: "success"}, ContextPointers: []apiv1.ContextPointer{{Name: "producer.artifact[0]", Artifact: &pointer}}}
	e.ArtifactChecks = map[string]ArtifactCheckFunc{"evidence": func(_ context.Context, inputs map[string]interface{}, params map[string]string, pointers []apiv1.ContextPointer, reader artifactset.Reader) (string, error) {
		if inputs[InputKeyStatus] != "success" || params["producer"] != "producer" || reader != r || len(pointers) != 1 || *pointers[0].Artifact != pointer {
			t.Fatal("artifact check lost its bounded inputs")
		}
		pointers[0].Artifact.Path = "mutated"
		return OutcomePass, nil
	}}
	if got, err := e.Evaluate(context.Background(), apiv1.AutomatedGate{Check: "status-equals"}, env); got != OutcomePass || err != nil || opened != 0 {
		t.Fatalf("scalar check used journal: %s, %v, %d", got, err, opened)
	}
	if got, err := e.Evaluate(context.Background(), apiv1.AutomatedGate{Check: "evidence", Params: map[string]string{"producer": "producer"}}, env); got != OutcomePass || err != nil || !r.closed || opened != 1 {
		t.Fatalf("artifact check: %s, %v, closed=%v", got, err, r.closed)
	}
	if pointer.Path != "artifacts/index" {
		t.Fatal("check mutated runner context pointer")
	}
}

func TestArtifactCheckFailures(t *testing.T) {
	infra := errors.New("journal unavailable")
	for _, failure := range []error{artifactset.ErrInvalid, infra} {
		r := &testArtifactReader{}
		e := &AutomatedEvaluator{ArtifactChecks: map[string]ArtifactCheckFunc{"evidence": func(context.Context, map[string]interface{}, map[string]string, []apiv1.ContextPointer, artifactset.Reader) (string, error) {
			return OutcomePass, failure
		}}, OpenArtifacts: func(context.Context, string, string) (ArtifactReader, error) { return r, nil }}
		got, err := e.Evaluate(context.Background(), apiv1.AutomatedGate{Check: "evidence"}, apiv1.InvocationEnvelope{})
		if !r.closed {
			t.Fatal("reader not closed")
		}
		if errors.Is(failure, artifactset.ErrInvalid) {
			if got != OutcomeFail || err != nil {
				t.Fatalf("invalid evidence: %s, %v", got, err)
			}
		} else if got != "" || !errors.Is(err, infra) {
			t.Fatalf("infrastructure error: %s, %v", got, err)
		}
	}
}

func TestArtifactCheckRegistryRefusesAmbiguity(t *testing.T) {
	e := NewAutomatedEvaluator()
	e.ArtifactChecks = map[string]ArtifactCheckFunc{"status-equals": nil}
	if _, err := e.Evaluate(context.Background(), apiv1.AutomatedGate{Check: "status-equals"}, apiv1.InvocationEnvelope{}); err == nil {
		t.Fatal("ambiguous registry accepted")
	}
}
