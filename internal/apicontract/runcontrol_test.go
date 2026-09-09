package apicontract

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRunControlWireOmitsServerAuthority(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  map[string]any
	}{
		{"trigger", TriggerRequest{Workflow: "impl", Force: true, Actor: "private", DispatchRunID: "assigned", PodScoped: true, PodRunID: "own"}, map[string]any{"workflow": "impl", "force": true}},
		{"cancel", CancelRunRequest{RunID: "route-run", IdempotencyKey: "header-key", Actor: "operator"}, map[string]any{"actor": "operator"}},
		{"acceptance without dispatch", TriggerResponse{AcceptanceID: "trigger-1", State: "accepted"}, map[string]any{"acceptanceId": "trigger-1", "state": "accepted"}},
		{"requested is not terminal", CancelRunResult{Code: "cancellation_requested"}, map[string]any{"code": "cancellation_requested"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("wire=%s want=%v", data, tt.want)
			}
		})
	}
}
