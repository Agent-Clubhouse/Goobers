package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

const actionsAppID = 15368

type supersededCheckCase struct {
	name      string
	checkRuns []map[string]interface{}
	// actionsRuns, when set, makes check-runs answer the fine-grained-PAT 403
	// so checkDetails takes the actions/runs fallback (#2685).
	actionsRuns []map[string]interface{}
	wantState   CheckState
	wantChecks  []CheckDetail
}

func checkRunFixture(id int64, name string, appID int64, status, conclusion string) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "name": name, "status": status, "conclusion": conclusion,
		"app": map[string]interface{}{"id": appID},
	}
}

func actionsRunFixture(id int64, name string, workflowID int64, status, conclusion string) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "name": name, "workflow_id": workflowID, "status": status,
		"conclusion": conclusion, "head_sha": "deadbeef",
	}
}

func passing(name string) CheckDetail {
	return CheckDetail{Name: name, State: CheckStatePassing, Conclusion: "success"}
}

func failing(name, conclusion string) CheckDetail {
	return CheckDetail{Name: name, State: CheckStateFailing, Conclusion: conclusion}
}

// TestGitHubProviderCheckDetailsDropsSupersededCancelledRun covers #6360: a
// cancel-in-progress workflow leaves a cancelled check run beside the newer
// run that superseded it, and that cancellation must not fail the PR. Only a
// superseded *cancelled* run is dropped; any other run still counts, so a
// same-named failure from another workflow is never hidden (#139).
func TestGitHubProviderCheckDetailsDropsSupersededCancelledRun(t *testing.T) {
	cases := []supersededCheckCase{
		{
			name: "superseded cancelled run does not fail a newer success",
			checkRuns: []map[string]interface{}{
				checkRunFixture(101, "delivery", actionsAppID, "completed", "cancelled"),
				checkRunFixture(102, "delivery", actionsAppID, "completed", "success"),
			},
			wantState:  CheckStatePassing,
			wantChecks: []CheckDetail{passing("delivery")},
		},
		{
			name: "newer run is chosen by id regardless of listing order",
			checkRuns: []map[string]interface{}{
				checkRunFixture(102, "delivery", actionsAppID, "completed", "success"),
				checkRunFixture(101, "delivery", actionsAppID, "completed", "cancelled"),
			},
			wantState:  CheckStatePassing,
			wantChecks: []CheckDetail{passing("delivery")},
		},
		{
			name: "superseded cancelled run beside a newer queued run stays pending",
			checkRuns: []map[string]interface{}{
				checkRunFixture(101, "unit", actionsAppID, "completed", "cancelled"),
				checkRunFixture(102, "unit", actionsAppID, "queued", ""),
			},
			wantState:  CheckStatePending,
			wantChecks: []CheckDetail{{Name: "unit", State: CheckStatePending}},
		},
		{
			name: "newest run cancelled still fails",
			checkRuns: []map[string]interface{}{
				checkRunFixture(101, "unit", actionsAppID, "completed", "success"),
				checkRunFixture(102, "unit", actionsAppID, "completed", "cancelled"),
			},
			wantState:  CheckStateFailing,
			wantChecks: []CheckDetail{passing("unit"), failing("unit", "cancelled")},
		},
		{
			name: "older same-named failure is never hidden by a newer success",
			checkRuns: []map[string]interface{}{
				checkRunFixture(101, "test", actionsAppID, "completed", "failure"),
				checkRunFixture(102, "test", actionsAppID, "completed", "success"),
			},
			wantState:  CheckStateFailing,
			wantChecks: []CheckDetail{failing("test", "failure"), passing("test")},
		},
		{
			name: "cancelled run is not superseded by a same-named check from another app",
			checkRuns: []map[string]interface{}{
				checkRunFixture(101, "lint", 1, "completed", "cancelled"),
				checkRunFixture(102, "lint", 2, "completed", "success"),
			},
			wantState:  CheckStateFailing,
			wantChecks: []CheckDetail{failing("lint", "cancelled"), passing("lint")},
		},
		{
			name: "distinct checks keep API order and worst-case-wins",
			checkRuns: []map[string]interface{}{
				checkRunFixture(101, "build", actionsAppID, "completed", "cancelled"),
				checkRunFixture(102, "test", actionsAppID, "completed", "failure"),
				checkRunFixture(103, "build", actionsAppID, "completed", "success"),
			},
			wantState:  CheckStateFailing,
			wantChecks: []CheckDetail{failing("test", "failure"), passing("build")},
		},
		{
			name: "actions fallback: superseded cancelled workflow run does not fail a newer success",
			actionsRuns: []map[string]interface{}{
				actionsRunFixture(201, "CI", 7, "completed", "cancelled"),
				actionsRunFixture(202, "CI", 7, "completed", "success"),
			},
			wantState:  CheckStatePassing,
			wantChecks: []CheckDetail{passing("CI")},
		},
		{
			name: "actions fallback: cancelled run of another workflow is not superseded",
			actionsRuns: []map[string]interface{}{
				actionsRunFixture(201, "CI", 7, "completed", "cancelled"),
				actionsRunFixture(202, "CI", 8, "completed", "success"),
			},
			wantState:  CheckStateFailing,
			wantChecks: []CheckDetail{failing("CI", "cancelled"), passing("CI")},
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

// TestGitHubProviderCIFailuresSkipsSupersededCancelledRun pins that
// CIFailures, which shares checkDetails, neither reports a superseded
// cancelled run as a failure nor fetches its annotations (#6360).
func TestGitHubProviderCIFailuresSkipsSupersededCancelledRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/commits/deadbeef/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"statuses": []map[string]interface{}{}})
	})
	mux.HandleFunc("/repos/acme/app/commits/deadbeef/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"check_runs": []map[string]interface{}{
			checkRunFixture(101, "delivery", actionsAppID, "completed", "cancelled"),
			checkRunFixture(102, "delivery", actionsAppID, "completed", "success"),
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
		t.Fatalf("failures = %+v, want none (the cancelled run was superseded by a newer success)", failures)
	}
}
