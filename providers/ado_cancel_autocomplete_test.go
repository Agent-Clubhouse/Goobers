package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// adoAutoCompleteServer serves pull request 42's detail: before (until a
// PATCH arrives) and after (once it has). It records every PATCH body.
func adoAutoCompleteServer(t *testing.T, before, after map[string]interface{}) (*ADOProvider, *[]map[string]interface{}) {
	t.Helper()
	var patches []map[string]interface{}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if len(patches) > 0 {
				writeJSON(t, w, after)
				return
			}
			writeJSON(t, w, before)
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

var (
	adoArmedDetail    = map[string]interface{}{"pullRequestId": 42, "status": "active", "autoCompleteSetBy": map[string]string{"id": "self-guid"}}
	adoDisarmedDetail = map[string]interface{}{"pullRequestId": 42, "status": "active"}
)

// TestADOCancelAutoCompleteDisarmsArmedPullRequest (#6134): an armed
// auto-complete is cleared with the empty identity and confirmed by a re-read.
func TestADOCancelAutoCompleteDisarmsArmedPullRequest(t *testing.T) {
	provider, patches := adoAutoCompleteServer(t, adoArmedDetail, adoDisarmedDetail)
	outcome, err := provider.CancelAutoComplete(context.Background(), adoLandingRepo(), "42")
	if err != nil || outcome != AutoCompleteCancelled {
		t.Fatalf("CancelAutoComplete = %q, %v; want cancelled", outcome, err)
	}
	if len(*patches) != 1 {
		t.Fatalf("patches = %v, want one", *patches)
	}
	setBy, _ := (*patches)[0]["autoCompleteSetBy"].(map[string]interface{})
	if setBy["id"] != adoNoAutoCompleteIdentity {
		t.Fatalf("patch = %v, want autoCompleteSetBy cleared to the empty identity", (*patches)[0])
	}
}

// The outcome is honest at the edges: nothing armed is a no-op; a PR that
// completed (before, or while, the cancel ran) is ErrLandedBeforeDisarm; a
// queued completion or a cancel that did not take is an error, never
// reported as cancelled.
func TestADOCancelAutoCompleteOutcomesAtTheEdges(t *testing.T) {
	completed := map[string]interface{}{"pullRequestId": 42, "status": "completed"}
	inFlight := map[string]interface{}{"pullRequestId": 42, "status": "active", "completionQueueTime": "2026-10-01T12:00:00Z", "mergeStatus": "queued"}

	provider, patches := adoAutoCompleteServer(t, adoDisarmedDetail, nil)
	if outcome, err := provider.CancelAutoComplete(context.Background(), adoLandingRepo(), "42"); err != nil || outcome != AutoCompleteNotArmed || len(*patches) != 0 {
		t.Fatalf("not armed: %q, %v (patches %v); want a no-op", outcome, err, *patches)
	}
	provider, patches = adoAutoCompleteServer(t, completed, nil)
	if _, err := provider.CancelAutoComplete(context.Background(), adoLandingRepo(), "42"); !errors.Is(err, ErrLandedBeforeDisarm) || len(*patches) != 0 {
		t.Fatalf("completed before: err = %v (patches %v); want ErrLandedBeforeDisarm, no patch", err, *patches)
	}
	provider, patches = adoAutoCompleteServer(t, inFlight, nil)
	if outcome, err := provider.CancelAutoComplete(context.Background(), adoLandingRepo(), "42"); err == nil || outcome != "" || len(*patches) != 0 {
		t.Fatalf("in-flight: %q, %v; want an error and no patch", outcome, err)
	}
	provider, _ = adoAutoCompleteServer(t, adoArmedDetail, completed)
	if _, err := provider.CancelAutoComplete(context.Background(), adoLandingRepo(), "42"); !errors.Is(err, ErrLandedBeforeDisarm) {
		t.Fatalf("completed during the cancel: err = %v, want ErrLandedBeforeDisarm", err)
	}
	provider, _ = adoAutoCompleteServer(t, adoArmedDetail, inFlight)
	if outcome, err := provider.CancelAutoComplete(context.Background(), adoLandingRepo(), "42"); err == nil || outcome == AutoCompleteCancelled {
		t.Fatalf("completion queued during the cancel: %q, %v; want an error", outcome, err)
	}
	provider, _ = adoAutoCompleteServer(t, adoArmedDetail, adoArmedDetail)
	if outcome, err := provider.CancelAutoComplete(context.Background(), adoLandingRepo(), "42"); err == nil || outcome == AutoCompleteCancelled {
		t.Fatalf("cancel did not take: %q, %v; want an error", outcome, err)
	}
}
