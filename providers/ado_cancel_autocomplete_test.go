package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// adoAutoCompleteServer serves pull request 42's detail and records the
// auto-complete PATCH CancelAutoComplete sends.
func adoAutoCompleteServer(t *testing.T, detail map[string]interface{}) (*ADOProvider, *[]map[string]interface{}) {
	t.Helper()
	var patches []map[string]interface{}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, detail)
		case http.MethodPatch:
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode patch: %v", err)
			}
			patches = append(patches, body)
			writeJSON(t, w, map[string]interface{}{"pullRequestId": 42, "status": "active"})
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL }), &patches
}

// TestADOCancelAutoCompleteDisarmsArmedPullRequest (#6134): an armed
// auto-complete is cleared with the empty identity.
func TestADOCancelAutoCompleteDisarmsArmedPullRequest(t *testing.T) {
	provider, patches := adoAutoCompleteServer(t, map[string]interface{}{
		"pullRequestId": 42, "status": "active", "autoCompleteSetBy": map[string]string{"id": "self-guid"},
	})
	disarmed, err := provider.CancelAutoComplete(context.Background(), adoLandingRepo(), "42")
	if err != nil || !disarmed {
		t.Fatalf("CancelAutoComplete = %v, %v; want disarmed", disarmed, err)
	}
	if len(*patches) != 1 {
		t.Fatalf("patches = %v, want one", *patches)
	}
	setBy, _ := (*patches)[0]["autoCompleteSetBy"].(map[string]interface{})
	if setBy["id"] != adoNoAutoCompleteIdentity {
		t.Fatalf("patch = %v, want autoCompleteSetBy cleared to the empty identity", (*patches)[0])
	}
}

// Nothing armed, or not active: no mutation. A completion ADO already queued
// cannot be recalled and is an error, never reported as disarmed.
func TestADOCancelAutoCompleteLeavesUnarmedAndRefusesInFlight(t *testing.T) {
	for name, detail := range map[string]map[string]interface{}{
		"not armed": {"pullRequestId": 42, "status": "active"},
		"completed": {"pullRequestId": 42, "status": "completed"},
	} {
		provider, patches := adoAutoCompleteServer(t, detail)
		disarmed, err := provider.CancelAutoComplete(context.Background(), adoLandingRepo(), "42")
		if err != nil || disarmed || len(*patches) != 0 {
			t.Fatalf("%s: CancelAutoComplete = %v, %v (patches %v); want a no-op", name, disarmed, err, *patches)
		}
	}
	provider, patches := adoAutoCompleteServer(t, map[string]interface{}{
		"pullRequestId": 42, "status": "active", "completionQueueTime": "2026-10-01T12:00:00Z", "mergeStatus": "queued",
	})
	if disarmed, err := provider.CancelAutoComplete(context.Background(), adoLandingRepo(), "42"); err == nil || disarmed || len(*patches) != 0 {
		t.Fatalf("in-flight completion: CancelAutoComplete = %v, %v; want an error and no patch", disarmed, err)
	}
}
