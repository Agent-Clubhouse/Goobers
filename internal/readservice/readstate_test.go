package readservice

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// TestEveryReadResponseCarriesTheEnvelope is #1927's acceptance criterion:
// "every read route's response carries readState".
//
// Written against the TYPES rather than by calling each method, because the
// guarantee is structural: a new response type added without the embed would
// pass any behavioural test that did not happen to cover it, and this fails
// instead.
func TestEveryReadResponseCarriesTheEnvelope(t *testing.T) {
	envelope := reflect.TypeOf(ReadStateEnvelope{})
	for _, response := range []any{
		Health{}, PortalConfig{}, Instance{}, GagglePage{}, GooberPage{},
		WorkflowPage{}, WorkflowDetail{}, GaggleConnections{},
		RunList{}, RunDetail{}, EventList{}, AttemptList{},
	} {
		typ := reflect.TypeOf(response)
		if _, found := typ.FieldByName("ReadState"); !found {
			t.Errorf("%s does not carry readState; a client cannot tell how current its "+
				"data is", typ.Name())
			continue
		}
		// Embedded, not a named field: embedding is what PROMOTES the JSON key
		// to the top level. A named field would nest it and every client would
		// read response.readStateEnvelope.readState instead.
		field, ok := typ.FieldByName("ReadStateEnvelope")
		if !ok || !field.Anonymous || field.Type != envelope {
			t.Errorf("%s does not embed ReadStateEnvelope; the readState key would nest "+
				"instead of appearing at the top level", typ.Name())
		}
	}
}

// TestEnvelopeSerialisesAtTheTopLevel pins the promotion, since the structural
// check above cannot prove the JSON shape.
func TestEnvelopeSerialisesAtTheTopLevel(t *testing.T) {
	list := RunList{}
	list.ReadState = &readmodel.ReadState{Epoch: "abc", LagSeconds: 1.5}

	encoded, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["readState"]; !ok {
		t.Errorf("readState is not a top-level key: %s", encoded)
	}
	if _, nested := decoded["ReadStateEnvelope"]; nested {
		t.Error("the embedded struct serialised as its own key rather than being promoted")
	}
}

// TestEnvelopeIsOmittedWithoutAReadModel pins that absence is absence.
//
// A zero-valued envelope would be worse than none: it reports epoch "" and lag
// 0, which reads as "perfectly current" rather than "unknown". The CLI and
// standalone topologies have no read model attached, and a daemon whose store
// failed to open has none either — all three must omit the field, not fake it.
func TestEnvelopeIsOmittedWithoutAReadModel(t *testing.T) {
	service := &Local{}
	got := service.readStateEnvelope(context.Background())
	if got.ReadState != nil {
		t.Errorf("a service with no read model produced an envelope: %+v", got.ReadState)
	}

	encoded, err := json.Marshal(RunList{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "readState") {
		t.Errorf("readState appears in a response with no read model: %s", encoded)
	}
}

// TestEnvelopeFailureDoesNotFailTheRequest pins that the annotation is
// best-effort.
//
// This is metadata ABOUT an answer that has already been computed successfully.
// Failing the request because the freshness annotation could not be assembled
// would let a diagnostic break the thing it exists to describe.
func TestEnvelopeFailureDoesNotFailTheRequest(t *testing.T) {
	service := &Local{}
	service.sources.ReadModel = brokenReader{}

	// A reader that is not a FreshnessReporter: the envelope is skipped rather
	// than erroring, because a backend that cannot describe its currency is
	// still a usable read model.
	got := service.readStateEnvelope(context.Background())
	if got.ReadState != nil {
		t.Error("a reader with no freshness surface produced an envelope")
	}
}

func TestRunningRunListDoesNotWaitForFreshnessAnnotation(t *testing.T) {
	model := blockingFreshnessReader{}
	service := &Local{
		sources:        LocalSources{ReadModel: model},
		now:            time.Now,
		readModelReads: true,
	}

	started := time.Now()
	list, err := service.ListRuns(context.Background(), RunListOptions{
		Phase: journal.PhaseRunning,
		Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("running run list waited %s for optional freshness metadata", elapsed)
	}
	if len(list.Runs) != 1 || list.Runs[0].Phase != journal.PhaseRunning {
		t.Fatalf("running run list = %+v", list.Runs)
	}
	if list.ReadState != nil {
		t.Fatalf("timed-out freshness metadata = %+v, want omitted", list.ReadState)
	}
}

// brokenReader implements just enough of readmodel.Reader to be attachable, and
// deliberately does NOT implement FreshnessReporter.
type brokenReader struct{ readmodel.Reader }

type blockingFreshnessReader struct{ readmodel.Reader }

func (blockingFreshnessReader) ListRuns(context.Context, readmodel.ListOptions) (readmodel.ListPage, error) {
	return readmodel.ListPage{Runs: []readmodel.RunRow{{Phase: journal.PhaseRunning}}}, nil
}

func (blockingFreshnessReader) ReadState(ctx context.Context, _ readmodel.ReadStateInput) (readmodel.ReadState, error) {
	<-ctx.Done()
	return readmodel.ReadState{}, ctx.Err()
}

func (blockingFreshnessReader) SourceApplied(context.Context, string) (readmodel.SourcePosition, bool, error) {
	return readmodel.SourcePosition{}, false, nil
}

func (blockingFreshnessReader) SatisfiesSourceApplied(context.Context, readmodel.SourcePosition) (bool, error) {
	return false, nil
}
