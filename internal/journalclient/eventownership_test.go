package journalclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPEventOwnershipIsBoundAndForeignEventsAreRefused(t *testing.T) {
	for _, owner := range []string{"", "run-1", "foreign"} {
		t.Run("owner="+owner, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"runId": "run-1", "events": []any{map[string]any{"type": "runner.annotation", "runId": owner}}})
			}))
			defer server.Close()
			client, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "fixture", RunID: "run-1"})
			if err != nil {
				t.Fatal(err)
			}
			events, err := client.Events()
			if owner == "foreign" {
				if err == nil || events != nil {
					t.Fatalf("foreign event accepted: %+v %v", events, err)
				}
			} else if err != nil || len(events) != 1 || events[0].RunID != "run-1" {
				t.Fatalf("event lost authenticated ownership: %+v %v", events, err)
			}
		})
	}
}
