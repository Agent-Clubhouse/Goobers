package eventing

import (
	"encoding/json"
	"strings"
	"testing"
)

func executionBundle(t *testing.T) ExecutionInputs {
	t.Helper()
	first, err := Parse([]byte(`{"specversion":"1.0","id":"1","source":"/test","type":"changed"}`))
	if err != nil {
		t.Fatal(err)
	}
	start := StartEnvelope{Kind: StartKind, Gaggle: "g", GroupID: "group", Consumer: "c", Revision: "r", Workflow: "w", WorkflowDigest: "wf", GooberDigest: "goober", ConfigGeneration: "generation", InputMode: "all", EventCount: 1}
	return ExecutionInputs{Manifest: InputManifest{Start: start, AcceptanceID: "trigger-run", Actor: "binding", Members: []InputMember{{ReceiptID: "event", Sequence: 1, Digest: first.Digest, Producer: Producer{Gaggle: "g", Binding: "binding", Actor: "operator"}, Selected: true}}}, Envelopes: map[string][]byte{"event": first.JSON}}
}

func TestExecutionBundleRejectsMixedScopeMissingSelectionAndMutableBytes(t *testing.T) {
	cases := map[string]func(*ExecutionInputs){
		"scope":     func(in *ExecutionInputs) { in.Manifest.Members[0].Producer.Gaggle = "other" },
		"producer":  func(in *ExecutionInputs) { in.Manifest.Members[0].Producer.RunID = "unpaired" },
		"missing":   func(in *ExecutionInputs) { delete(in.Envelopes, "event") },
		"extra":     func(in *ExecutionInputs) { in.Envelopes["foreign"] = in.Envelopes["event"] },
		"selection": func(in *ExecutionInputs) { in.Manifest.Members[0].Selected = false },
		"tamper": func(in *ExecutionInputs) {
			in.Envelopes["event"] = []byte(strings.ReplaceAll(string(in.Envelopes["event"]), "changed", "other"))
		},
		"duplicate": func(in *ExecutionInputs) {
			in.Manifest.Start.EventCount = 2
			in.Manifest.Members = append(in.Manifest.Members, in.Manifest.Members[0])
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			in := executionBundle(t)
			edit(&in)
			if _, err := in.Validate("run", "g"); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	in := executionBundle(t)
	raw, err := in.Validate("run", "g")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["endpoint"] = "foreign"
	raw, err = json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ParseInputManifest(raw); err == nil {
		t.Fatal("unrecognized persisted authority accepted")
	}
}
