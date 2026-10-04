package journal

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/validate"
)

func sessionIdentity() RunIdentity {
	id := eventIdentity()
	id.Event = nil
	id.Trigger = Trigger{Kind: TriggerSignal, Ref: "session:session:turn"}
	id.Session = &SessionLineage{Gaggle: id.Gaggle, SessionID: "session", TurnID: "turn", MessageID: "message", AcceptanceID: "trigger-" + id.RunID, EnvelopeDigest: Digest([]byte("envelope")), InputDigest: Digest([]byte("input"))}
	return id
}

func TestSessionLineageClosedSchemaAndMixedProvenance(t *testing.T) {
	id := sessionIdentity()
	validator, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	run, err := Create(t.TempDir(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	rd, err := OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	actual, err := rd.Identity()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(actual)
	if err = validator.ValidateJSON("journal-run.schema.json", raw); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["session"].(map[string]any)["credential"] = "forbidden"
	raw, _ = json.Marshal(doc)
	if err = validator.ValidateJSON("journal-run.schema.json", raw); err == nil {
		t.Fatal("open session schema")
	}
	for name, mutate := range map[string]func(*RunIdentity){"foreign": func(id *RunIdentity) { id.Session.Gaggle = "other" }, "child": func(id *RunIdentity) { id.Child = &ChildLineage{} }, "event": func(id *RunIdentity) { id.Event = &EventLineage{} }, "receipt": func(id *RunIdentity) { id.Session.AcceptanceID = "trigger-" + strings.Repeat("c", 32) }, "digest": func(id *RunIdentity) { id.Session.InputDigest = "changed" }} {
		t.Run(name, func(t *testing.T) {
			id := sessionIdentity()
			mutate(&id)
			if _, err := Create(filepath.Join(t.TempDir(), "runs"), id, nil); err == nil {
				t.Fatal("invalid lineage created")
			}
		})
	}
}
