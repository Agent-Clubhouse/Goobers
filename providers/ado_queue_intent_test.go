package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestADOAutoCompleteIntentAndAcknowledgement(t *testing.T) {
	for _, scenario := range []string{"armed", "storage-failure", "already-armed", "unacknowledged", "different-identity"} {
		t.Run(scenario, func(t *testing.T) {
			r := &intentTestRecorder{}
			failure := errors.New("intent unavailable")
			if scenario == "storage-failure" {
				r.err = failure
			}
			var mutations atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				out := map[string]any{"pullRequestId": 42, "status": "active", "createdBy": map[string]string{"id": "creator"}, "lastMergeSourceCommit": map[string]string{"commitId": "head"}}
				if req.Method == http.MethodPatch {
					mutations.Add(1)
					if !r.ready.Load() {
						t.Error("auto-complete mutation preceded persisted intent")
					}
					switch scenario {
					case "armed":
						out["autoCompleteSetBy"] = map[string]string{"id": "creator"}
					case "different-identity":
						out["autoCompleteSetBy"] = map[string]string{"id": "other"}
					}
				} else if scenario == "already-armed" {
					out["autoCompleteSetBy"] = map[string]string{"id": "creator"}
				}
				writeJSON(t, w, out)
			}))
			defer server.Close()
			p := NewADOProvider("org", "project", "fixture", func(p *ADOProvider) { p.BaseURL = server.URL })
			p.SetMutationRecorder(r)
			result, err := p.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{Repository: adoLandingRepo(), PullID: "42", ExpectedHeadSHA: "head"})
			if scenario == "storage-failure" {
				if !errors.Is(err, failure) || mutations.Load() != 0 {
					t.Fatalf("persistence failure allowed mutation: %v calls=%d", err, mutations.Load())
				}
			} else if err != nil {
				t.Fatal(err)
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
