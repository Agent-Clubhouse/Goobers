package validate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/hostedprogress"
	"github.com/goobers/goobers/internal/journal"
)

// minimalHostedProgressContract returns a Contract populated with only the
// fields the published JSON schema requires. Every published projection can
// be reduced to (or expanded from) this shape without breaking the wire
// contract — so schema strictness is validated against the same struct
// callers actually marshal (not an ad-hoc JSON blob).
func minimalHostedProgressContract(t *testing.T) hostedprogress.Contract {
	t.Helper()
	startedAt, err := time.Parse(time.RFC3339, "2026-07-21T12:00:00Z")
	if err != nil {
		t.Fatalf("parse startedAt: %v", err)
	}
	updatedAt, err := time.Parse(time.RFC3339, "2026-07-21T12:00:30Z")
	if err != nil {
		t.Fatalf("parse updatedAt: %v", err)
	}
	return hostedprogress.Contract{
		Schema:        hostedprogress.Schema,
		Revision:      1,
		ActionsRunID:  "42",
		ActionsRunURL: "https://github.com/example/repo/actions/runs/42",
		Identity: journal.RunIdentity{
			Schema:          journal.RunSchema,
			RunID:           "run-abc",
			Workflow:        "default-implement",
			WorkflowVersion: 1,
			Gaggle:          "example",
			Trigger:         journal.Trigger{Kind: journal.TriggerManual},
			StartedAt:       startedAt,
		},
		Phase: journal.PhaseRunning,
		Events: []journal.Event{{
			Schema: journal.EventSchema,
			Seq:    1,
			Type:   journal.EventRunStarted,
			Time:   startedAt,
		}},
		UpdatedAt: updatedAt,
	}
}

func TestHostedProgressInProgressContractValidates(t *testing.T) {
	contract := minimalHostedProgressContract(t)
	data, err := json.Marshal(contract)
	if err != nil {
		t.Fatalf("marshal hosted-progress contract: %v", err)
	}
	if err := newV(t).ValidateJSON(schemas.HostedProgress, data); err != nil {
		t.Fatalf("in-progress hosted-progress contract should validate: %v\n%s", err, data)
	}
}

func TestHostedProgressTerminalContractValidates(t *testing.T) {
	contract := minimalHostedProgressContract(t)
	contract.Revision = 12
	contract.Phase = journal.PhaseCompleted
	contract.Events = append(contract.Events, journal.Event{
		Schema: journal.EventSchema,
		Seq:    12,
		Type:   journal.EventRunFinished,
		Time:   contract.UpdatedAt,
	})
	data, err := json.Marshal(contract)
	if err != nil {
		t.Fatalf("marshal terminal hosted-progress contract: %v", err)
	}
	if err := newV(t).ValidateJSON(schemas.HostedProgress, data); err != nil {
		t.Fatalf("terminal hosted-progress contract should validate: %v\n%s", err, data)
	}
}

func TestHostedProgressTruncatedContractValidates(t *testing.T) {
	// Shape a producer actually emits after payload bounding drops middle
	// projected events: the anchor Events[0] (typically run.started) is
	// retained at its original low seq, later events remain, and
	// TruncatedBefore records the highest dropped seq — strictly above the
	// anchor seq and strictly below the retained trailing event's seq. See
	// TestBoundContractMarksTruncationWhenTwoEventsBecomeOne in
	// internal/hostedprogress for the pinned producer contract.
	contract := minimalHostedProgressContract(t)
	contract.Revision = 200
	contract.TruncatedBefore = 150
	contract.Graph = json.RawMessage(`{"nodes":[]}`)
	contract.Events = append(contract.Events, journal.Event{
		Schema: journal.EventSchema,
		Seq:    200,
		Type:   journal.EventRunFinished,
		Time:   contract.UpdatedAt,
	})
	data, err := json.Marshal(contract)
	if err != nil {
		t.Fatalf("marshal truncated hosted-progress contract: %v", err)
	}
	if err := newV(t).ValidateJSON(schemas.HostedProgress, data); err != nil {
		t.Fatalf("truncated hosted-progress contract should validate: %v\n%s", err, data)
	}
}

// TestHostedProgressAllEventsDroppedContractValidates covers the shape a
// producer actually emits when payload bounding drops every projected event
// (see TestBoundContractMarksAllEventsDropped in internal/hostedprogress):
// TruncatedBefore is set to Revision, and Events is an empty non-nil slice
// so the payload marshals to `"events": []` rather than `"events": null`.
// The published schema declares events as required with `"type": "array"`, so
// a nil slice would fail validation; this test locks the wire contract for
// the state producer/docs/schema all describe as legitimate.
func TestHostedProgressAllEventsDroppedContractValidates(t *testing.T) {
	contract := minimalHostedProgressContract(t)
	contract.Revision = 200
	contract.TruncatedBefore = 200
	contract.Events = []journal.Event{}
	data, err := json.Marshal(contract)
	if err != nil {
		t.Fatalf("marshal all-dropped hosted-progress contract: %v", err)
	}
	if want := `"events":[]`; !strings.Contains(string(data), want) {
		t.Fatalf("marshaled payload must contain %s, got: %s", want, data)
	}
	if err := newV(t).ValidateJSON(schemas.HostedProgress, data); err != nil {
		t.Fatalf("all-events-dropped hosted-progress contract should validate: %v\n%s", err, data)
	}
}

func TestHostedProgressSchemaIsClosedAndVersioned(t *testing.T) {
	v := newV(t)
	for name, mutate := range map[string]func(*map[string]json.RawMessage){
		"unknown top-level field": func(raw *map[string]json.RawMessage) {
			(*raw)["futureSection"] = json.RawMessage(`{}`)
		},
		"wrong schema constant": func(raw *map[string]json.RawMessage) {
			(*raw)["schema"] = json.RawMessage(`"goobers.dev/hosted-progress/v2"`)
		},
		"revision below minimum": func(raw *map[string]json.RawMessage) {
			(*raw)["revision"] = json.RawMessage(`0`)
		},
		"unknown phase": func(raw *map[string]json.RawMessage) {
			(*raw)["phase"] = json.RawMessage(`"bogus"`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(minimalHostedProgressContract(t))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			mutate(&raw)
			mutated, err := json.Marshal(raw)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			if err := v.ValidateJSON(schemas.HostedProgress, mutated); err == nil {
				t.Fatalf("expected hosted-progress schema to reject %s", name)
			}
		})
	}
}
