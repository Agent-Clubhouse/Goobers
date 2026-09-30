package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// typedPolicy builds an enabled, blocking policy evaluation of a well-known
// ADO policy type (ADO-N19 classifies by type id, not display name).
func typedPolicy(typeID, displayName, status string) map[string]interface{} {
	return map[string]interface{}{
		"status": status,
		"configuration": map[string]interface{}{
			"isEnabled":  true,
			"isBlocking": true,
			"type":       map[string]string{"id": typeID, "displayName": displayName},
		},
	}
}

// buildPolicy is a build-validation evaluation carrying context.buildId.
func buildPolicy(status string, buildID int) map[string]interface{} {
	ev := typedPolicy(adoPolicyTypeBuild, "Build", status)
	ev["context"] = map[string]interface{}{"buildId": buildID, "isExpired": false}
	return ev
}

func pollADOPolicies(t *testing.T, evaluations []map[string]interface{}) PullRequestPollResult {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", prDetailHandler(t, nil))
	mux.HandleFunc("/org/project/_apis/policy/evaluations", policyEvaluationsHandler(t, evaluations))
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	result, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		PullID:     "42",
	})
	if err != nil {
		t.Fatalf("PollPullRequest returned error: %v", err)
	}
	return result
}

func checkNamed(t *testing.T, checks []CheckDetail, name string) CheckDetail {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %+v", name, checks)
	return CheckDetail{}
}

// TestADOPollPullRequestClassifiesPolicyEvaluations is ADO-N19 (design
// ado-parity-dsl-2-0.md §5.1): evaluations are classified by policy type id.
func TestADOPollPullRequestClassifiesPolicyEvaluations(t *testing.T) {
	t.Run("queued reviewer policy is a human wait, not CI pending", func(t *testing.T) {
		result := pollADOPolicies(t, []map[string]interface{}{
			buildPolicy("approved", 7),
			typedPolicy(adoPolicyTypeMinimumReviewers, "Minimum number of reviewers", "queued"),
		})
		if result.CheckState != CheckStatePassing {
			t.Fatalf("CheckState = %q, want passing — a queued reviewer policy is not CI pending", result.CheckState)
		}
		reviewer := checkNamed(t, result.Checks, "Minimum number of reviewers")
		if !reviewer.AwaitingHuman || reviewer.State != CheckStatePending {
			t.Fatalf("reviewer check = %+v, want pending and AwaitingHuman", reviewer)
		}
	})
	t.Run("only reviewer policies evaluated is not CI pending", func(t *testing.T) {
		result := pollADOPolicies(t, []map[string]interface{}{
			typedPolicy(adoPolicyTypeRequiredReviewers, "Required reviewers", "queued"),
		})
		if result.CheckState != CheckStatePassing {
			t.Fatalf("CheckState = %q, want passing — there is no CI to wait for", result.CheckState)
		}
	})
	t.Run("rejected reviewer policy never fails CI", func(t *testing.T) {
		result := pollADOPolicies(t, []map[string]interface{}{
			buildPolicy("approved", 7),
			typedPolicy(adoPolicyTypeMinimumReviewers, "Minimum number of reviewers", "rejected"),
		})
		if result.CheckState != CheckStatePassing {
			t.Fatalf("CheckState = %q, want passing — a reviewer vote is never a remediation trigger", result.CheckState)
		}
		for _, c := range result.Checks {
			if c.State == CheckStateFailing {
				t.Fatalf("check %+v is failing; reviewer policies must never report failing", c)
			}
		}
	})
	t.Run("running build with queued reviewer is CI pending", func(t *testing.T) {
		result := pollADOPolicies(t, []map[string]interface{}{
			buildPolicy("running", 7),
			typedPolicy(adoPolicyTypeMinimumReviewers, "Minimum number of reviewers", "queued"),
		})
		if result.CheckState != CheckStatePending {
			t.Fatalf("CheckState = %q, want pending", result.CheckState)
		}
	})
	t.Run("rejected build is failing with the build recorded", func(t *testing.T) {
		result := pollADOPolicies(t, []map[string]interface{}{
			buildPolicy("rejected", 1234),
			typedPolicy(adoPolicyTypeMinimumReviewers, "Minimum number of reviewers", "queued"),
		})
		if result.CheckState != CheckStateFailing {
			t.Fatalf("CheckState = %q, want failing", result.CheckState)
		}
		build := checkNamed(t, result.Checks, "Build")
		if build.State != CheckStateFailing || !strings.HasSuffix(build.URL, "/org/project/_build/results?buildId=1234") {
			t.Fatalf("build check = %+v, want failing with a link to buildId 1234", build)
		}
	})
	t.Run("broken status policy is failing", func(t *testing.T) {
		result := pollADOPolicies(t, []map[string]interface{}{
			typedPolicy(adoPolicyTypeStatus, "Status", "broken"),
		})
		if result.CheckState != CheckStateFailing {
			t.Fatalf("CheckState = %q, want failing", result.CheckState)
		}
	})
	// "Require a merge strategy" is settled by the completion's mergeStrategy,
	// not by code: before completion ADO reports it unmet, and treating that as
	// failing CI escalated every implementation run on a repository with the
	// policy (found in the v0.5.0 ADO soak).
	t.Run("merge-strategy policy never gates CI", func(t *testing.T) {
		for _, status := range []string{"rejected", "queued"} {
			result := pollADOPolicies(t, []map[string]interface{}{
				buildPolicy("approved", 7),
				typedPolicy("fa4e907d-c16b-4a4c-9dfa-4916e5d171ab", "Require a merge strategy", status),
			})
			if result.CheckState != CheckStatePassing {
				t.Fatalf("status %s: CheckState = %q, want passing — the build passed and a merge strategy is not CI", status, result.CheckState)
			}
			for _, c := range result.Checks {
				if c.State == CheckStateFailing {
					t.Fatalf("status %s: check %+v is failing; a merge-strategy policy must never fail CI", status, c)
				}
			}
		}
		pending := pollADOPolicies(t, []map[string]interface{}{
			buildPolicy("queued", 7),
			typedPolicy("fa4e907d-c16b-4a4c-9dfa-4916e5d171ab", "Require a merge strategy", "rejected"),
		})
		if pending.CheckState != CheckStatePending {
			t.Fatalf("queued build + unmet merge strategy: CheckState = %q, want pending (the build is still running), not failing", pending.CheckState)
		}
	})
	// PO ruling 2026-09-27: a rejected comment-resolution or
	// work-item-linking policy is not failing CI. Its threads route to
	// gather-review-threads and its link to open-pr's workItemRefs.
	t.Run("rejected comment and work-item policies say why without failing CI", func(t *testing.T) {
		result := pollADOPolicies(t, []map[string]interface{}{
			buildPolicy("approved", 7),
			typedPolicy(adoPolicyTypeCommentRequirements, "Comment requirements", "rejected"),
			typedPolicy(adoPolicyTypeWorkItemLinking, "Work item linking", "rejected"),
		})
		if result.CheckState != CheckStatePassing {
			t.Fatalf("CheckState = %q, want passing — the build passed and the other policies are not CI", result.CheckState)
		}
		for name, summary := range map[string]string{
			"Comment requirements": "unresolved comment threads",
			"Work item linking":    "no linked work item",
		} {
			check := checkNamed(t, result.Checks, name)
			if check.Summary != summary || check.State != CheckStatePending || check.Conclusion != "rejected" {
				t.Fatalf("%s check = %+v, want pending, conclusion rejected, summary %q", name, check, summary)
			}
		}
	})
	t.Run("only rejected comment and work-item policies is not CI failing", func(t *testing.T) {
		result := pollADOPolicies(t, []map[string]interface{}{
			typedPolicy(adoPolicyTypeCommentRequirements, "Comment requirements", "rejected"),
			typedPolicy(adoPolicyTypeWorkItemLinking, "Work item linking", "broken"),
		})
		if result.CheckState != CheckStatePassing {
			t.Fatalf("CheckState = %q, want passing — there is no CI to fail", result.CheckState)
		}
	})
	t.Run("rejected comment policy does not mask a failing build", func(t *testing.T) {
		result := pollADOPolicies(t, []map[string]interface{}{
			buildPolicy("rejected", 7),
			typedPolicy(adoPolicyTypeCommentRequirements, "Comment requirements", "rejected"),
		})
		if result.CheckState != CheckStateFailing {
			t.Fatalf("CheckState = %q, want failing from the build", result.CheckState)
		}
	})
	t.Run("rejected comment policy does not settle a running build", func(t *testing.T) {
		result := pollADOPolicies(t, []map[string]interface{}{
			buildPolicy("running", 7),
			typedPolicy(adoPolicyTypeCommentRequirements, "Comment requirements", "rejected"),
		})
		if result.CheckState != CheckStatePending {
			t.Fatalf("CheckState = %q, want pending on the build", result.CheckState)
		}
	})
}

// detectPolicyServer serves a PR detail, its policy evaluations and a paged
// configurations list. pages[i] is served for the i-th continuation token;
// every page but the last carries x-ms-continuationtoken.
type detectPolicyServer struct {
	evaluations []map[string]interface{}
	pages       [][]map[string]interface{}
	evalCalls   int64
	cfgCalls    int64
}

func (s *detectPolicyServer) start(t *testing.T) *ADOProvider {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", prDetailHandler(t, nil))
	mux.HandleFunc("/org/project/_apis/policy/evaluations", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&s.evalCalls, 1)
		if got := r.URL.Query().Get("api-version"); got != "7.1-preview.1" {
			t.Errorf("evaluations api-version = %q, want 7.1-preview.1", got)
		}
		policyEvaluationsHandler(t, s.evaluations)(w, r)
	})
	mux.HandleFunc("/org/project/_apis/policy/configurations", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&s.cfgCalls, 1)
		index := 0
		if token := r.URL.Query().Get("continuationToken"); token != "" {
			index = int(token[len(token)-1] - '0')
		}
		if index < len(s.pages)-1 {
			w.Header().Set("x-ms-continuationtoken", "page"+string(rune('0'+index+1)))
		}
		var page []map[string]interface{}
		if index < len(s.pages) {
			page = s.pages[index]
		}
		writeJSON(t, w, map[string]interface{}{"value": page})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
}

func blockingConfig(scope ...map[string]string) map[string]interface{} {
	return map[string]interface{}{
		"isEnabled": true, "isBlocking": true, "isDeleted": false,
		"settings": map[string]interface{}{"scope": scope},
	}
}

// TestADOProviderDetectMergePolicyFromEvaluations is ADO-N19: with a PullID
// the pull request's own evaluations decide enqueue versus direct, and the
// configuration scan is only the fallback.
func TestADOProviderDetectMergePolicyFromEvaluations(t *testing.T) {
	tests := []struct {
		name        string
		evaluations []map[string]interface{}
		pages       [][]map[string]interface{}
		want        MergePolicy
		wantCfgScan bool
	}{
		{
			name: "unmet reviewer evaluation arms auto-complete",
			evaluations: []map[string]interface{}{
				buildPolicy("approved", 7),
				typedPolicy(adoPolicyTypeMinimumReviewers, "Minimum number of reviewers", "queued"),
			},
			want: MergePolicyMergeQueue,
		},
		{
			name: "every evaluation approved or not applicable completes directly",
			evaluations: []map[string]interface{}{
				buildPolicy("approved", 7),
				typedPolicy(adoPolicyTypeWorkItemLinking, "Work item linking", "notApplicable"),
			},
			// A configuration scan would say merge_queue; the evaluations win.
			pages: [][]map[string]interface{}{{blockingConfig(map[string]string{"refName": "refs/heads/master"})}},
			want:  MergePolicyDirect,
		},
		{
			name:        "no evaluations falls back to the paged configuration scan",
			evaluations: nil,
			pages: [][]map[string]interface{}{
				{blockingConfig(map[string]string{"refName": "refs/heads/other"})},
				{blockingConfig(map[string]string{"refName": "refs/heads/master"})},
			},
			want:        MergePolicyMergeQueue,
			wantCfgScan: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := &detectPolicyServer{evaluations: tc.evaluations, pages: tc.pages}
			provider := srv.start(t)
			result, err := provider.DetectMergePolicy(context.Background(), RepoMergePolicyRequest{
				Repository: adoLandingRepo(), Branch: "master", PullID: "42",
			})
			if err != nil {
				t.Fatalf("DetectMergePolicy returned error: %v", err)
			}
			if result.Policy != tc.want {
				t.Fatalf("Policy = %q, want %q", result.Policy, tc.want)
			}
			if atomic.LoadInt64(&srv.evalCalls) != 1 {
				t.Fatalf("evaluations read %d times, want 1", srv.evalCalls)
			}
			if scanned := atomic.LoadInt64(&srv.cfgCalls) > 0; scanned != tc.wantCfgScan {
				t.Fatalf("configuration scan ran = %v, want %v", scanned, tc.wantCfgScan)
			}
		})
	}
}

// TestADOProviderDetectMergePolicyConfigurationScan covers the fallback scan
// (design §5.1): it pages through x-ms-continuationtoken, matches Prefix scopes
// by ref folder, and treats an empty scope as repo-wide.
func TestADOProviderDetectMergePolicyConfigurationScan(t *testing.T) {
	tests := []struct {
		name   string
		branch string
		pages  [][]map[string]interface{}
		want   MergePolicy
	}{
		{
			name:   "blocking policy on the second page is found",
			branch: "main",
			pages: [][]map[string]interface{}{
				{blockingConfig(map[string]string{"refName": "refs/heads/other"})},
				{blockingConfig(map[string]string{"refName": "refs/heads/main"})},
			},
			want: MergePolicyMergeQueue,
		},
		{
			name:   "prefix scope matches a branch in its folder",
			branch: "release/1.0",
			pages:  [][]map[string]interface{}{{blockingConfig(map[string]string{"refName": "refs/heads/release/", "matchKind": "Prefix"})}},
			want:   MergePolicyMergeQueue,
		},
		{
			name:   "prefix scope on refs/heads/ covers every branch",
			branch: "main",
			pages:  [][]map[string]interface{}{{blockingConfig(map[string]string{"refName": "refs/heads/", "matchKind": "Prefix"})}},
			want:   MergePolicyMergeQueue,
		},
		{
			name:   "prefix scope does not match outside its folder",
			branch: "released",
			pages:  [][]map[string]interface{}{{blockingConfig(map[string]string{"refName": "refs/heads/release", "matchKind": "Prefix"})}},
			want:   MergePolicyDirect,
		},
		{
			name:   "empty scope is repo-wide",
			branch: "main",
			pages:  [][]map[string]interface{}{{blockingConfig()}},
			want:   MergePolicyMergeQueue,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := &detectPolicyServer{pages: tc.pages}
			provider := srv.start(t)
			result, err := provider.DetectMergePolicy(context.Background(), RepoMergePolicyRequest{Repository: adoLandingRepo(), Branch: tc.branch})
			if err != nil {
				t.Fatalf("DetectMergePolicy returned error: %v", err)
			}
			if result.Policy != tc.want {
				t.Fatalf("Policy = %q, want %q", result.Policy, tc.want)
			}
			if atomic.LoadInt64(&srv.evalCalls) != 0 {
				t.Fatal("evaluations were read without a PullID")
			}
		})
	}
}

// TestADOProviderPollMergeQueueEntryAwaitingHuman is ADO-N19: an armed,
// active pull request held only by a reviewer policy is reported pending and
// AwaitingHuman; one still waiting on CI is plain pending.
func TestADOProviderPollMergeQueueEntryAwaitingHuman(t *testing.T) {
	tests := []struct {
		name        string
		evaluations []map[string]interface{}
		want        bool
	}{
		{
			name: "reviewer-only hold",
			evaluations: []map[string]interface{}{
				buildPolicy("approved", 7),
				typedPolicy(adoPolicyTypeRequiredReviewers, "Required reviewers", "queued"),
			},
			want: true,
		},
		{
			name: "build still running",
			evaluations: []map[string]interface{}{
				buildPolicy("running", 7),
				typedPolicy(adoPolicyTypeRequiredReviewers, "Required reviewers", "queued"),
			},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]interface{}{
					"pullRequestId": 42, "status": "active",
					"autoCompleteSetBy": map[string]string{"id": "someone"},
					"repository": map[string]interface{}{
						"name": "repo", "project": map[string]string{"id": "proj-guid", "name": "project"},
					},
				})
			})
			mux.HandleFunc("/org/project/_apis/policy/evaluations", policyEvaluationsHandler(t, tc.evaluations))
			server := httptest.NewServer(mux)
			defer server.Close()

			provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			result, err := provider.PollMergeQueueEntry(context.Background(), PollMergeQueueEntryRequest{Repository: adoLandingRepo(), PullID: "42"})
			if err != nil {
				t.Fatalf("PollMergeQueueEntry returned error: %v", err)
			}
			if result.State != MergeQueueEntryPending {
				t.Fatalf("State = %q, want pending — an unmet reviewer policy is never an eviction", result.State)
			}
			if result.AwaitingHuman != tc.want {
				t.Fatalf("AwaitingHuman = %v, want %v", result.AwaitingHuman, tc.want)
			}
		})
	}
}
