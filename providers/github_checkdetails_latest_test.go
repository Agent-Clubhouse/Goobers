package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

type checkDetailsLatestCase struct {
	name      string
	checkRuns []map[string]interface{}
	// actionsRuns, when set, makes check-runs answer the fine-grained-PAT 403
	// so checkDetails takes the actions/runs fallback (#2685).
	actionsRuns []map[string]interface{}
	wantState   CheckState
	wantChecks  []CheckDetail
}

func checkRunFixture(id int64, name string, appID int64, startedAt, status, conclusion string) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "name": name, "status": status, "conclusion": conclusion,
		"started_at": startedAt, "app": map[string]interface{}{"id": appID},
	}
}

func actionsRunFixture(id int64, name string, workflowID int64, createdAt, status, conclusion string) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "name": name, "workflow_id": workflowID, "status": status,
		"conclusion": conclusion, "created_at": createdAt, "head_sha": "deadbeef",
	}
}

// TestGitHubProviderCheckDetailsCountsOnlyLatestRunPerCheck covers #6360: a
// cancel-in-progress workflow leaves a cancelled check run beside the newer run
// that superseded it, and only the newer run may decide the PR's check state.
func TestGitHubProviderCheckDetailsCountsOnlyLatestRunPerCheck(t *testing.T) {
	const (
		earlier = "2026-10-01T10:00:00Z"
		later   = "2026-10-01T10:05:00Z"
	)
	cases := []checkDetailsLatestCase{
		{
			name: "superseded cancelled run does not fail a newer success",
			checkRuns: []map[string]interface{}{
				checkRunFixture(101, "delivery", 15368, earlier, "completed", "cancelled"),
				checkRunFixture(102, "delivery", 15368, later, "completed", "success"),
			},
			wantState:  CheckStatePassing,
			wantChecks: []CheckDetail{{Name: "delivery", State: CheckStatePassing, Conclusion: "success"}},
		},
		{
			name: "newest run wins regardless of listing order",
			checkRuns: []map[string]interface{}{
				checkRunFixture(102, "delivery", 15368, later, "completed", "success"),
				checkRunFixture(101, "delivery", 15368, earlier, "completed", "cancelled"),
			},
			wantState:  CheckStatePassing,
			wantChecks: []CheckDetail{{Name: "delivery", State: CheckStatePassing, Conclusion: "success"}},
		},
		{
			name: "newer failure supersedes an older success",
			checkRuns: []map[string]interface{}{
				checkRunFixture(101, "unit", 15368, earlier, "completed", "success"),
				checkRunFixture(102, "unit", 15368, later, "completed", "failure"),
			},
			wantState:  CheckStateFailing,
			wantChecks: []CheckDetail{{Name: "unit", State: CheckStateFailing, Conclusion: "failure"}},
		},
		{
			name: "newer in-progress run keeps the check pending",
			checkRuns: []map[string]interface{}{
				checkRunFixture(101, "unit", 15368, earlier, "completed", "cancelled"),
				checkRunFixture(102, "unit", 15368, later, "in_progress", ""),
			},
			wantState:  CheckStatePending,
			wantChecks: []CheckDetail{{Name: "unit", State: CheckStatePending}},
		},
		{
			name: "equal start times fall back to the later run id",
			checkRuns: []map[string]interface{}{
				checkRunFixture(102, "unit", 15368, earlier, "completed", "success"),
				checkRunFixture(101, "unit", 15368, earlier, "completed", "cancelled"),
			},
			wantState:  CheckStatePassing,
			wantChecks: []CheckDetail{{Name: "unit", State: CheckStatePassing, Conclusion: "success"}},
		},
		{
			name: "same name from different apps are distinct checks",
			checkRuns: []map[string]interface{}{
				checkRunFixture(101, "lint", 1, earlier, "completed", "failure"),
				checkRunFixture(102, "lint", 2, later, "completed", "success"),
			},
			wantState: CheckStateFailing,
			wantChecks: []CheckDetail{
				{Name: "lint", State: CheckStateFailing, Conclusion: "failure"},
				{Name: "lint", State: CheckStatePassing, Conclusion: "success"},
			},
		},
		{
			name: "distinct checks keep first-seen order and worst-case-wins",
			checkRuns: []map[string]interface{}{
				checkRunFixture(101, "build", 15368, earlier, "completed", "cancelled"),
				checkRunFixture(102, "test", 15368, earlier, "completed", "failure"),
				checkRunFixture(103, "build", 15368, later, "completed", "success"),
			},
			wantState: CheckStateFailing,
			wantChecks: []CheckDetail{
				{Name: "build", State: CheckStatePassing, Conclusion: "success"},
				{Name: "test", State: CheckStateFailing, Conclusion: "failure"},
			},
		},
		{
			name: "actions fallback: superseded cancelled workflow run does not fail a newer success",
			actionsRuns: []map[string]interface{}{
				actionsRunFixture(201, "CI", 7, earlier, "completed", "cancelled"),
				actionsRunFixture(202, "CI", 7, later, "completed", "success"),
			},
			wantState:  CheckStatePassing,
			wantChecks: []CheckDetail{{Name: "CI", State: CheckStatePassing, Conclusion: "success"}},
		},
		{
			name: "actions fallback: same name from different workflows are distinct",
			actionsRuns: []map[string]interface{}{
				actionsRunFixture(201, "CI", 7, earlier, "completed", "failure"),
				actionsRunFixture(202, "CI", 8, later, "completed", "success"),
			},
			wantState: CheckStateFailing,
			wantChecks: []CheckDetail{
				{Name: "CI", State: CheckStateFailing, Conclusion: "failure"},
				{Name: "CI", State: CheckStatePassing, Conclusion: "success"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/acme/app/commits/deadbeef/status", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]interface{}{"statuses": []map[string]interface{}{}})
			})
			mux.HandleFunc("/repos/acme/app/commits/deadbeef/check-runs", func(w http.ResponseWriter, _ *http.Request) {
				if tc.actionsRuns != nil {
					w.WriteHeader(http.StatusForbidden)
					writeJSON(t, w, map[string]interface{}{"message": "Resource not accessible by personal access token"})
					return
				}
				writeJSON(t, w, map[string]interface{}{"check_runs": tc.checkRuns})
			})
			mux.HandleFunc("/repos/acme/app/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]interface{}{"workflow_runs": tc.actionsRuns})
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
			state, details, err := provider.combinedCheckState(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "deadbeef")
			if err != nil {
				t.Fatalf("combinedCheckState: %v", err)
			}
			if state != tc.wantState {
				t.Fatalf("state = %q, want %q", state, tc.wantState)
			}
			if !reflect.DeepEqual(details, tc.wantChecks) {
				t.Fatalf("details = %+v, want %+v", details, tc.wantChecks)
			}
		})
	}
}

// TestGitHubProviderCIFailuresSkipsSupersededRun pins that CIFailures, which
// shares checkDetails, neither reports a superseded cancelled run as a failure
// nor fetches its annotations (#6360).
func TestGitHubProviderCIFailuresSkipsSupersededRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/commits/deadbeef/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"statuses": []map[string]interface{}{}})
	})
	mux.HandleFunc("/repos/acme/app/commits/deadbeef/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"check_runs": []map[string]interface{}{
			checkRunFixture(101, "delivery", 15368, "2026-10-01T10:00:00Z", "completed", "cancelled"),
			checkRunFixture(102, "delivery", 15368, "2026-10-01T10:05:00Z", "completed", "success"),
		}})
	})
	mux.HandleFunc("/repos/acme/app/check-runs/101/annotations", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("fetched annotations for the superseded run 101")
		writeJSON(t, w, []map[string]interface{}{})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	failures, err := provider.CIFailures(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "deadbeef")
	if err != nil {
		t.Fatalf("CIFailures: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("failures = %+v, want none (the cancelled run was superseded by a success)", failures)
	}
}
