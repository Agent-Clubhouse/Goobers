package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newADOBranchTestProvider(t *testing.T, mux *http.ServeMux) *ADOProvider {
	t.Helper()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
}

// TestADOProviderListBranchesIsBoundedAndCursorable proves ListBranches
// follows every refs page, sorts lexically regardless of server order, keeps
// only refs/heads/<prefix>* names, and honours After and Limit (#5900).
func TestADOProviderListBranchesIsBoundedAndCursorable(t *testing.T) {
	var tokens []string
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/refs", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		if got := r.URL.Query().Get("filter"); got != "heads/goobers/" {
			t.Fatalf("filter = %q, want heads/goobers/", got)
		}
		token := r.URL.Query().Get("continuationToken")
		tokens = append(tokens, token)
		switch token {
		case "":
			w.Header().Set("x-ms-continuationtoken", "page-2")
			writeJSON(t, w, map[string]interface{}{"value": []map[string]string{
				{"name": "refs/heads/goobers/d", "objectId": "sha-d"},
				{"name": "refs/heads/goobers/a", "objectId": "sha-a"},
			}})
		case "page-2":
			writeJSON(t, w, map[string]interface{}{"value": []map[string]string{
				{"name": "refs/heads/goobers/c", "objectId": "sha-c", "url": "https://example.test/c"},
				{"name": "refs/heads/goobers/b", "objectId": "sha-b"},
				{"name": "refs/tags/goobers/z", "objectId": "sha-z"},
			}})
		default:
			t.Fatalf("unexpected continuation token %q", token)
		}
	})
	provider := newADOBranchTestProvider(t, mux)

	branches, err := provider.ListBranches(context.Background(), ListBranchesRequest{
		Repository: adoLandingRepo(), Prefix: "goobers/", After: "goobers/a", Limit: 2,
	})
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	want := []BranchSummary{
		{Name: "goobers/b", SHA: "sha-b"},
		{Name: "goobers/c", SHA: "sha-c", URL: "https://example.test/c"},
	}
	if !reflect.DeepEqual(branches, want) {
		t.Fatalf("branches = %#v, want %#v", branches, want)
	}
	if !reflect.DeepEqual(tokens, []string{"", "page-2"}) {
		t.Fatalf("continuation tokens = %q, want both pages read", tokens)
	}
}

func TestADOProviderListBranchesValidatesBound(t *testing.T) {
	provider := NewADOProvider("org", "project", "token")
	if _, err := provider.ListBranches(context.Background(), ListBranchesRequest{Repository: adoLandingRepo(), Limit: 1}); err == nil {
		t.Fatal("ListBranches without prefix returned nil error")
	}
	if _, err := provider.ListBranches(context.Background(), ListBranchesRequest{Repository: adoLandingRepo(), Prefix: "goobers/"}); err == nil {
		t.Fatal("ListBranches without limit returned nil error")
	}
}

func TestADOProviderListBranchesRejectsRepeatedContinuationToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/refs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ms-continuationtoken", "same")
		writeJSON(t, w, map[string]interface{}{"value": []map[string]string{}})
	})
	provider := newADOBranchTestProvider(t, mux)
	_, err := provider.ListBranches(context.Background(), ListBranchesRequest{Repository: adoLandingRepo(), Prefix: "goobers/", Limit: 5})
	if err == nil || !strings.Contains(err.Error(), "continuation token repeated") {
		t.Fatalf("error = %v, want repeated continuation token failure", err)
	}
}

// adoBranchRefsHandler serves a starts-with refs filter result in which a
// longer sibling sorts before the exact ref, as ADO's prefix filter allows.
func adoBranchRefsHandler(t *testing.T, refs []map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Get("filter") != "heads/goobers/run-1" {
			t.Fatalf("filter = %q, want heads/goobers/run-1", r.URL.Query().Get("filter"))
		}
		writeJSON(t, w, map[string]interface{}{"value": refs})
	}
}

// TestADOProviderGetBranchMatchesExactRefAndReadsLastPush proves GetBranch
// ignores a prefix-matched sibling and reports the newest push to the exact
// ref as its activity time.
func TestADOProviderGetBranchMatchesExactRefAndReadsLastPush(t *testing.T) {
	pushedAt := time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/refs", adoBranchRefsHandler(t, []map[string]string{
		{"name": "refs/heads/goobers/run-10", "objectId": "sibling-sha"},
		{"name": "refs/heads/goobers/run-1", "objectId": "exact-sha", "url": "https://example.test/ref"},
	}))
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pushes", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		q := r.URL.Query()
		if q.Get("searchCriteria.refName") != "refs/heads/goobers/run-1" || q.Get("$top") != "1" || q.Get("searchCriteria.includeRefUpdates") != "true" {
			t.Fatalf("pushes query = %q", r.URL.RawQuery)
		}
		writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{{
			"pushId": 7, "date": pushedAt.Format(time.RFC3339),
			"refUpdates": []map[string]string{{"name": "refs/heads/goobers/run-1"}},
		}}})
	})
	provider := newADOBranchTestProvider(t, mux)

	branch, found, err := provider.GetBranch(context.Background(), adoLandingRepo(), "goobers/run-1")
	if err != nil || !found {
		t.Fatalf("GetBranch = found %v, err %v; want found", found, err)
	}
	if branch.Name != "goobers/run-1" || branch.SHA != "exact-sha" || branch.URL != "https://example.test/ref" {
		t.Fatalf("branch = %#v, want the exact ref", branch)
	}
	if branch.LastActivityAt == nil || !branch.LastActivityAt.Equal(pushedAt) {
		t.Fatalf("LastActivityAt = %v, want %v", branch.LastActivityAt, pushedAt)
	}
}

// TestADOProviderGetBranchReportsAbsentWhenOnlySiblingMatches proves a
// prefix-matched sibling is never mistaken for the requested branch.
func TestADOProviderGetBranchReportsAbsentWhenOnlySiblingMatches(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/refs", adoBranchRefsHandler(t, []map[string]string{
		{"name": "refs/heads/goobers/run-10", "objectId": "sibling-sha"},
	}))
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pushes", func(http.ResponseWriter, *http.Request) {
		t.Fatal("pushes read for an absent branch")
	})
	provider := newADOBranchTestProvider(t, mux)

	if _, found, err := provider.GetBranch(context.Background(), adoLandingRepo(), "goobers/run-1"); err != nil || found {
		t.Fatalf("GetBranch = found %v, err %v; want absent without error", found, err)
	}
}

// TestADOProviderGetBranchActivity pins how the push read maps onto
// LastActivityAt: no recorded push leaves activity unknown (reconciliation
// then preserves the branch), and a push for another ref is an error rather
// than borrowed activity.
func TestADOProviderGetBranchActivity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pushes  []map[string]interface{}
		wantErr bool
	}{
		{name: "no pushes", pushes: []map[string]interface{}{}},
		{name: "push for another ref", pushes: []map[string]interface{}{{
			"pushId": 9, "date": "2026-09-14T10:30:00Z",
			"refUpdates": []map[string]string{{"name": "refs/heads/main"}},
		}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/org/project/_apis/git/repositories/repo/refs", adoBranchRefsHandler(t, []map[string]string{
				{"name": "refs/heads/goobers/run-1", "objectId": "exact-sha"},
			}))
			mux.HandleFunc("/org/project/_apis/git/repositories/repo/pushes", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]interface{}{"value": tc.pushes})
			})
			provider := newADOBranchTestProvider(t, mux)

			branch, found, err := provider.GetBranch(context.Background(), adoLandingRepo(), "goobers/run-1")
			if tc.wantErr {
				if err == nil || found {
					t.Fatalf("GetBranch = found %v, err %v; want error", found, err)
				}
				return
			}
			if err != nil || !found || branch.SHA != "exact-sha" || branch.LastActivityAt != nil {
				t.Fatalf("GetBranch = %#v, found %v, err %v; want found with unknown activity", branch, found, err)
			}
		})
	}
}

// TestADOProviderDeleteBranchResolvesExactRefTip proves an unconditional
// delete leases the exact ref's tip, not a prefix-matched sibling's.
func TestADOProviderDeleteBranchResolvesExactRefTip(t *testing.T) {
	var postedOldObjectID string
	mux := http.NewServeMux()
	refs := adoBranchRefsHandler(t, []map[string]string{
		{"name": "refs/heads/goobers/run-10", "objectId": "sibling-sha"},
		{"name": "refs/heads/goobers/run-1", "objectId": "exact-sha"},
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/refs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			refs(w, r)
			return
		}
		var posted []map[string]string
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		postedOldObjectID = posted[0]["oldObjectId"]
		writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{{"name": "refs/heads/goobers/run-1", "success": true, "updateStatus": "succeeded"}}})
	})
	provider := newADOBranchTestProvider(t, mux)

	result, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{Repository: adoLandingRepo(), Name: "goobers/run-1"})
	if err != nil || !result.Deleted {
		t.Fatalf("DeleteBranch = %#v, err %v; want deleted", result, err)
	}
	if postedOldObjectID != "exact-sha" {
		t.Fatalf("oldObjectId = %q, want exact-sha", postedOldObjectID)
	}
}

// TestADOProviderDeleteBranchHonoursPerRefUpdateResult proves a ref update
// ADO rejects inside an HTTP 200 is never reported as a deletion: a stale
// oldObjectId is a lost lease, any other status a failure.
func TestADOProviderDeleteBranchHonoursPerRefUpdateResult(t *testing.T) {
	for _, status := range []string{"staleOldObjectId", "rejectedByPolicy"} {
		t.Run(status, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/org/project/_apis/git/repositories/repo/refs", func(w http.ResponseWriter, r *http.Request) {
				assertMethod(t, r, http.MethodPost)
				writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{{"name": "refs/heads/feature", "success": false, "updateStatus": status}}})
			})
			provider := newADOBranchTestProvider(t, mux)

			result, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{Repository: adoLandingRepo(), Name: "feature", ExpectedSHA: "sha1"})
			if err == nil || result.Deleted {
				t.Fatalf("DeleteBranch = %#v, err %v; want failure", result, err)
			}
			var tipChanged *BranchTipChangedError
			if isStale := errors.As(err, &tipChanged); isStale != (status == "staleOldObjectId") {
				t.Fatalf("error = %v; BranchTipChangedError = %v, want %v", err, isStale, status == "staleOldObjectId")
			}
		})
	}
}
