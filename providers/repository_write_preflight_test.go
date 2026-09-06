package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPreflightRepositoryWriteOKWhenPushableAndNoBlockingRule proves the
// success path: push permission granted and no branch_name_pattern rule
// present at all.
func TestPreflightRepositoryWriteOKWhenPushableAndNoBlockingRule(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{
			"permissions": map[string]interface{}{"push": true},
		})
	})
	mux.HandleFunc("/repos/acme/app/rules/branches/goobers/run-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]interface{}{
			{"type": "required_status_checks"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.PreflightRepositoryWrite(context.Background(),
		RepositoryRef{Owner: "acme", Name: "app"}, "goobers/run-1")
	if err != nil {
		t.Fatalf("PreflightRepositoryWrite returned error: %v", err)
	}
	if !result.OK || result.FailureCapability != "" {
		t.Fatalf("result = %+v, want OK with no failure capability", result)
	}
}

// TestPreflightRepositoryWriteReportsUnauthorizedOnRepoFetchFailure proves
// the repository-unreachable-or-unauthorized state: a 401/404/403 from the
// repo GET is classified explicitly, never silently treated as a passing
// check.
func TestPreflightRepositoryWriteReportsUnauthorizedOnRepoFetchFailure(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusForbidden} {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/app", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		})
		server := httptest.NewServer(mux)

		provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
		result, err := provider.PreflightRepositoryWrite(context.Background(),
			RepositoryRef{Owner: "acme", Name: "app"}, "goobers/run-1")
		server.Close()
		if err != nil {
			t.Fatalf("status %d: unexpected error: %v", status, err)
		}
		if result.OK || result.FailureCapability != RepoWriteFailureUnauthorized {
			t.Fatalf("status %d: result = %+v, want FailureCapability %q", status, result, RepoWriteFailureUnauthorized)
		}
	}
}

// TestPreflightRepositoryWriteReportsNoPushPermission proves the
// authenticated-but-lacking-push-permission state is distinguished from an
// unauthorized credential: the repo is reachable and the credential is
// valid, but permissions.push is false.
func TestPreflightRepositoryWriteReportsNoPushPermission(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"permissions": map[string]interface{}{"push": false},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.PreflightRepositoryWrite(context.Background(),
		RepositoryRef{Owner: "acme", Name: "app"}, "goobers/run-1")
	if err != nil {
		t.Fatalf("PreflightRepositoryWrite returned error: %v", err)
	}
	if result.OK || result.FailureCapability != RepoWriteFailureNoPushPermission {
		t.Fatalf("result = %+v, want FailureCapability %q", result, RepoWriteFailureNoPushPermission)
	}
}

// TestPreflightRepositoryWriteReportsPermissionIntrospectionUnavailable
// proves the fourth, distinct state: GitHub answered but did not report
// `permissions` at all — this must be reported as introspection-unavailable,
// never silently inferred as either a pass or a push-permission denial.
func TestPreflightRepositoryWriteReportsPermissionIntrospectionUnavailable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.PreflightRepositoryWrite(context.Background(),
		RepositoryRef{Owner: "acme", Name: "app"}, "goobers/run-1")
	if err != nil {
		t.Fatalf("PreflightRepositoryWrite returned error: %v", err)
	}
	if result.OK || result.FailureCapability != RepoWriteFailurePolicyIntrospectionUnavailable {
		t.Fatalf("result = %+v, want FailureCapability %q", result, RepoWriteFailurePolicyIntrospectionUnavailable)
	}
}

// TestPreflightRepositoryWriteReportsRulesIntrospectionUnavailableOn403
// proves the ruleset-introspection-unavailable case is distinguished from
// "no ruleset blocks this branch": a 403 on the rules-for-branch endpoint
// (the same plan-entitlement gap DetectMergePolicy tolerates) must be
// reported explicitly rather than treated as an all-clear.
func TestPreflightRepositoryWriteReportsRulesIntrospectionUnavailableOn403(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"permissions": map[string]interface{}{"push": true},
		})
	})
	mux.HandleFunc("/repos/acme/app/rules/branches/goobers/run-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.PreflightRepositoryWrite(context.Background(),
		RepositoryRef{Owner: "acme", Name: "app"}, "goobers/run-1")
	if err != nil {
		t.Fatalf("PreflightRepositoryWrite returned error: %v", err)
	}
	if result.OK || result.FailureCapability != RepoWriteFailurePolicyIntrospectionUnavailable {
		t.Fatalf("result = %+v, want FailureCapability %q", result, RepoWriteFailurePolicyIntrospectionUnavailable)
	}
}

// TestPreflightRepositoryWriteEvaluatesBranchNamePatternParameters is the
// direct regression for the prior attempt's defect: a matching
// branch_name_pattern rule in the "rules for a branch" response is NOT an
// unconditional denial — it must be evaluated against its own
// operator/pattern/negate parameters and the actual generated branch name.
func TestPreflightRepositoryWriteEvaluatesBranchNamePatternParameters(t *testing.T) {
	tests := []struct {
		name       string
		params     map[string]interface{}
		branch     string
		wantBlocks bool
	}{
		{
			name:       "starts_with matches and blocks",
			params:     map[string]interface{}{"operator": "starts_with", "pattern": "release/"},
			branch:     "release/1.0",
			wantBlocks: true,
		},
		{
			name:       "starts_with does not match so the rule does not block",
			params:     map[string]interface{}{"operator": "starts_with", "pattern": "release/"},
			branch:     "goobers/run-1",
			wantBlocks: false,
		},
		{
			name:       "ends_with matches and blocks",
			params:     map[string]interface{}{"operator": "ends_with", "pattern": "-hotfix"},
			branch:     "goobers/run-1-hotfix",
			wantBlocks: true,
		},
		{
			name:       "contains matches and blocks",
			params:     map[string]interface{}{"operator": "contains", "pattern": "protected"},
			branch:     "goobers/protected-run",
			wantBlocks: true,
		},
		{
			name:       "regex matches and blocks",
			params:     map[string]interface{}{"operator": "regex", "pattern": "^release/.+$"},
			branch:     "release/1.0",
			wantBlocks: true,
		},
		{
			name:       "regex does not match so the rule does not block",
			params:     map[string]interface{}{"operator": "regex", "pattern": "^release/.+$"},
			branch:     "goobers/run-1",
			wantBlocks: false,
		},
		{
			// A permissive branch-name rule is exactly the false-positive
			// scenario the prior attempt's blanket denial caused.
			name:       "negated rule permits generated namespace so it does not block",
			params:     map[string]interface{}{"operator": "starts_with", "pattern": "goobers/", "negate": true},
			branch:     "goobers/run-1",
			wantBlocks: false,
		},
		{
			name:       "negated rule blocks branches outside the permitted namespace",
			params:     map[string]interface{}{"operator": "starts_with", "pattern": "goobers/", "negate": true},
			branch:     "other/run-1",
			wantBlocks: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/acme/app", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, map[string]interface{}{
					"permissions": map[string]interface{}{"push": true},
				})
			})
			mux.HandleFunc("/repos/acme/app/rules/branches/"+tc.branch, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, []map[string]interface{}{
					{"type": "branch_name_pattern", "parameters": tc.params},
				})
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
			result, err := provider.PreflightRepositoryWrite(context.Background(),
				RepositoryRef{Owner: "acme", Name: "app"}, tc.branch)
			if err != nil {
				t.Fatalf("PreflightRepositoryWrite returned error: %v", err)
			}
			if tc.wantBlocks {
				if result.OK || result.FailureCapability != RepoWriteFailureBranchPolicy {
					t.Fatalf("result = %+v, want FailureCapability %q", result, RepoWriteFailureBranchPolicy)
				}
			} else if !result.OK {
				t.Fatalf("result = %+v, want OK (rule does not match this branch)", result)
			}
		})
	}
}

// TestPreflightRepositoryWriteUnrecognizedOperatorDoesNotBlock proves an
// operator this code does not yet know how to evaluate fails toward "does
// not block" rather than the prior defect's failure-mode of refusing every
// branch it cannot evaluate.
func TestPreflightRepositoryWriteUnrecognizedOperatorDoesNotBlock(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"permissions": map[string]interface{}{"push": true},
		})
	})
	mux.HandleFunc("/repos/acme/app/rules/branches/goobers/run-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]interface{}{
			{"type": "branch_name_pattern", "parameters": map[string]interface{}{"operator": "some_future_operator", "pattern": "x"}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.PreflightRepositoryWrite(context.Background(),
		RepositoryRef{Owner: "acme", Name: "app"}, "goobers/run-1")
	if err != nil {
		t.Fatalf("PreflightRepositoryWrite returned error: %v", err)
	}
	if !result.OK {
		t.Fatalf("result = %+v, want OK for an unrecognized operator", result)
	}
}

// TestPreflightRepositoryWriteRequiresBranch mirrors
// TestGitHubProviderDetectMergePolicyRequiresBranch's usage-error guard.
func TestPreflightRepositoryWriteRequiresBranch(t *testing.T) {
	provider := NewGitHubProvider("token")
	if _, err := provider.PreflightRepositoryWrite(context.Background(),
		RepositoryRef{Owner: "acme", Name: "app"}, ""); err == nil {
		t.Fatal("expected an error for a missing branch")
	}
}

// TestPreflightRepositoryWriteNeverMutates proves the acceptance criterion
// "never mutates repository state": every request the check issues is a
// GET; any other method fails the test.
func TestPreflightRepositoryWriteNeverMutates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("non-GET request to %s %s: preflight must never mutate repository state", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/repos/acme/app":
			writeJSON(t, w, map[string]interface{}{"permissions": map[string]interface{}{"push": true}})
		case "/repos/acme/app/rules/branches/goobers/run-1":
			writeJSON(t, w, []map[string]interface{}{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	if _, err := provider.PreflightRepositoryWrite(context.Background(),
		RepositoryRef{Owner: "acme", Name: "app"}, "goobers/run-1"); err != nil {
		t.Fatalf("PreflightRepositoryWrite returned error: %v", err)
	}
}

// TestDispatcherPreflightRepositoryWriteFailsClosedWithoutDeclaration proves
// the Dispatcher's fail-closed contract (§3.2): a provider kind that has not
// declared repo.push.preflight (or does not implement
// RepositoryWritePreflighter) never silently reports success.
func TestDispatcherPreflightRepositoryWriteFailsClosedWithoutDeclaration(t *testing.T) {
	provider := &GiteaProvider{}
	dispatcher := NewDispatcher(provider)
	_, err := dispatcher.PreflightRepositoryWrite(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "goobers/run-1")
	if err == nil {
		t.Fatal("expected ErrUnsupported for a provider that has not declared repo.push.preflight")
	}
	var unsupported ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
	if unsupported.Capability != CapRepoPushPreflight {
		t.Fatalf("Capability = %q, want %q", unsupported.Capability, CapRepoPushPreflight)
	}
}

// TestDispatcherPreflightRepositoryWriteDispatchesToGitHub proves the
// success side of the same dispatch contract.
func TestDispatcherPreflightRepositoryWriteDispatchesToGitHub(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"permissions": map[string]interface{}{"push": true}})
	})
	mux.HandleFunc("/repos/acme/app/rules/branches/goobers/run-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]interface{}{})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	dispatcher := NewDispatcher(provider)
	result, err := dispatcher.PreflightRepositoryWrite(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "goobers/run-1")
	if err != nil {
		t.Fatalf("Dispatcher.PreflightRepositoryWrite returned error: %v", err)
	}
	if !result.OK {
		t.Fatalf("result = %+v, want OK", result)
	}
}
