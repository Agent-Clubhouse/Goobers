package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func TestADODeferralPublishesHoldAndPreservesEvidence(t *testing.T) {
	for _, reason := range []apiv1.VerdictReasonCode{apiv1.VerdictReasonOrdering, apiv1.VerdictReasonNoLander} {
		t.Run(string(reason), func(t *testing.T) {
			var label, state, comment string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/statuses"):
					var body struct{ State string }
					_ = json.NewDecoder(r.Body).Decode(&body)
					state = body.State
					_, _ = w.Write([]byte(`{"id":7}`))
				case strings.HasSuffix(r.URL.Path, "/labels"):
					if r.Method == http.MethodGet {
						_, _ = w.Write([]byte(`{"value":[]}`))
						return
					}
					var body struct{ Name string }
					_ = json.NewDecoder(r.Body).Decode(&body)
					label = body.Name
					_, _ = w.Write([]byte(`{"id":"hold"}`))
				case strings.HasSuffix(r.URL.Path, "/threads"):
					var body struct{ Comments []struct{ Content string } }
					_ = json.NewDecoder(r.Body).Decode(&body)
					if len(body.Comments) > 0 {
						comment = body.Comments[0].Content
					}
					_, _ = w.Write([]byte(`{"id":11,"comments":[{"id":1,"content":"posted","author":{"displayName":"goober"},"publishedDate":"2026-08-09T00:00:00Z"}]}`))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			provider := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) { p.BaseURL = server.URL })
			verdict := apiv1.Verdict{Decision: apiv1.VerdictDefer, ReasonCode: reason, Rationale: "  Wait for ordering.\n\nKeep the full rationale.  ", Findings: []apiv1.Finding{{Severity: apiv1.SeverityInfo, Message: "Original finding."}}, HeadSHA: "head", BaseSHA: "base"}
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "scheduler"), 0o755); err != nil {
				t.Fatal(err)
			}
			resultFile := filepath.Join(root, "result.json")
			var stdout, stderr bytes.Buffer
			code := publishADONonPassVerdict(context.Background(), root, provider, providers.RepositoryRef{Provider: providers.ProviderADO, Project: "project", Name: "repo"}, 359, providers.PullRequestSummary{Number: 359, HeadSHA: "head", BaseSHA: "base"}, verdict, resultFile, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if label != blockedOnSiblingLabel || state != "failed" {
				t.Fatalf("label=%q state=%q", label, state)
			}
			got, ok := parseVerdictComment(comment)
			if !ok || !reflect.DeepEqual(got, verdict) {
				t.Fatalf("published evidence changed: %+v", got)
			}
			result := readVerdictResult(t, resultFile)
			if result["decision"] != "defer" || result["reason"] != string(reason) {
				t.Fatalf("lost disposition: %+v", result)
			}
		})
	}
}
