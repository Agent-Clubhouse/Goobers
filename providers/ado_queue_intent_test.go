package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestADOAutoCompleteIntentAndAcknowledgement drives EnqueuePullRequest
// through each acknowledgement outcome. The fixture's caller ("caller", from
// connectionData) is deliberately distinct from the PR's creator ("creator"):
// ADO accepts only the credential's own identity as autoCompleteSetBy (live
// probe F3), so a regression that fell back to the PR's createdBy would send
// the wrong id, and one that compared the acknowledgement against createdBy
// would claim an auto-complete the creator armed.
func TestADOAutoCompleteIntentAndAcknowledgement(t *testing.T) {
	const caller, creator = "caller", "creator"
	for _, scenario := range []string{"armed", "storage-failure", "already-armed", "unacknowledged", "creator-identity", "different-pr", "identity-unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			r := &intentTestRecorder{}
			failure := errors.New("intent unavailable")
			if scenario == "storage-failure" {
				r.err = failure
			}
			var mutations atomic.Int64
			var setBy atomic.Value
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if strings.HasSuffix(req.URL.Path, "/connectionData") {
					if scenario == "identity-unavailable" {
						http.Error(w, "unavailable", http.StatusInternalServerError)
						return
					}
					writeJSON(t, w, map[string]any{"authenticatedUser": map[string]any{"id": caller}})
					return
				}
				out := map[string]any{"pullRequestId": 42, "status": "active", "createdBy": map[string]string{"id": creator}, "lastMergeSourceCommit": map[string]string{"commitId": "head"}}
				if req.Method == http.MethodPatch {
					mutations.Add(1)
					if !r.ready.Load() {
						t.Error("auto-complete mutation preceded persisted intent")
					}
					var body struct {
						AutoCompleteSetBy struct {
							ID string `json:"id"`
						} `json:"autoCompleteSetBy"`
					}
					decodeJSON(t, req, &body)
					setBy.Store(body.AutoCompleteSetBy.ID)
					switch scenario {
					case "armed":
						out["autoCompleteSetBy"] = map[string]string{"id": caller}
					case "creator-identity":
						// Armed, but by the PR's creator rather than the
						// caller: not this caller's acknowledgement.
						out["autoCompleteSetBy"] = map[string]string{"id": creator}
					case "different-pr":
						out["pullRequestId"] = 43
						out["autoCompleteSetBy"] = map[string]string{"id": caller}
					}
				} else if scenario == "already-armed" {
					out["autoCompleteSetBy"] = map[string]string{"id": caller}
				}
				writeJSON(t, w, out)
			}))
			defer server.Close()
			p := NewADOProvider("org", "project", "fixture", func(p *ADOProvider) {
				p.BaseURL = server.URL
				p.sleep = func(context.Context, time.Duration) error { return nil }
			})
			p.SetMutationRecorder(r)
			result, err := p.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{Repository: adoLandingRepo(), PullID: "42", ExpectedHeadSHA: "head"})
			assertADOEnqueueScenarioError(t, scenario, err, failure, mutations.Load(), r)
			if id, _ := setBy.Load().(string); mutations.Load() > 0 && id != caller {
				t.Fatalf("PATCH autoCompleteSetBy.id = %q, want the caller %q (never the PR creator)", id, caller)
			}
			ref, recorded := r.last()
			if result.Merged || result.QueueEntryID != "" || ref.MergeConfirmation != nil || ref.QueueAdmission != nil {
				t.Fatalf("invented merge or queue entry: %+v %+v", result, ref)
			}
			if scenario == "armed" {
				if mutations.Load() != 1 || !recorded || ref.Operation != "enqueue" || ref.LandingIntent == nil || ref.LandingIntent.ID != r.intent.ID || r.intent.Operation != "enqueue" {
					t.Fatalf("missing acknowledged intent: %+v %+v calls=%d", r.intent, ref, mutations.Load())
				}
			} else if recorded {
				t.Fatalf("unproven auto-complete mutation recorded: %+v", ref)
			}
			if scenario == "already-armed" && (mutations.Load() != 0 || r.intent.ID != "") {
				t.Fatalf("existing auto-complete was claimed: %+v calls=%d", r.intent, mutations.Load())
			}
		})
	}
}

// assertADOEnqueueScenarioError checks EnqueuePullRequest's error for one
// TestADOAutoCompleteIntentAndAcknowledgement scenario.
func assertADOEnqueueScenarioError(t *testing.T, scenario string, err, storageFailure error, mutations int64, r *intentTestRecorder) {
	t.Helper()
	switch scenario {
	case "storage-failure":
		if !errors.Is(err, storageFailure) || mutations != 0 {
			t.Fatalf("persistence failure allowed mutation: %v calls=%d", err, mutations)
		}
	case "different-pr":
		if err == nil {
			t.Fatal("mismatched PR response accepted")
		}
	case "identity-unavailable":
		// Without the caller's identity there is no id ADO would accept, so
		// nothing is armed and no intent is persisted.
		if err == nil || !strings.Contains(err.Error(), "resolve authenticated identity") {
			t.Fatalf("err = %v, want the identity resolution failure", err)
		}
		if mutations != 0 || r.intent.ID != "" {
			t.Fatalf("enqueue without an identity patched or persisted intent: calls=%d intent=%+v", mutations, r.intent)
		}
	default:
		if err != nil {
			t.Fatal(err)
		}
	}
}
