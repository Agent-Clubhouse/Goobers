package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	testToken    = "test-token-value"
	testRepoID   = "11111111-2222-3333-4444-555555555555"
	reviewersID  = "type-reviewers"
	statusTypeID = "type-status"
)

// fakeADO serves the handful of Azure DevOps endpoints provision touches and
// records every request it receives.
type fakeADO struct {
	mu       sync.Mutex
	requests []string
	policies []map[string]any
	fixture  int
	nextID   int
	authOK   bool
}

func newFakeADO() *fakeADO { return &fakeADO{nextID: 100, authOK: true} }

func (f *fakeADO) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != basicAuth(testToken) {
		f.authOK = false
	}
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/example-org/example-project/_apis/git/repositories/example-scratch":
		writeJSON(w, map[string]string{"id": testRepoID, "name": "example-scratch"})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/_apis/policy/types"):
		writeJSON(w, map[string]any{"value": []map[string]string{
			{"id": reviewersID, "displayName": "Minimum number of reviewers"},
			{"id": statusTypeID, "displayName": "Status"},
		}})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/_apis/policy/configurations"):
		writeJSON(w, map[string]any{"value": f.policies})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_apis/policy/configurations"):
		var created map[string]any
		_ = json.Unmarshal(body, &created)
		f.nextID++
		created["id"] = f.nextID
		created["type"] = map[string]any{"id": created["type"].(map[string]any)["id"], "displayName": "created"}
		f.policies = append(f.policies, created)
		writeJSON(w, created)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_apis/wit/wiql"):
		items := []map[string]int{}
		if f.fixture > 0 {
			items = append(items, map[string]int{"id": f.fixture})
		}
		writeJSON(w, map[string]any{"workItems": items})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_apis/wit/workitems/$Issue"):
		f.nextID++
		f.fixture = f.nextID
		writeJSON(w, map[string]int{"id": f.fixture})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeADO) creates() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var creates []string
	for _, request := range f.requests {
		if strings.HasPrefix(request, http.MethodPost) && !strings.HasSuffix(request, "/wiql") {
			creates = append(creates, request)
		}
	}
	return creates
}

func runProvision(t *testing.T, server *httptest.Server, extra ...string) (int, string, string) {
	t.Helper()
	args := append([]string{"provision",
		"-organization-url", server.URL + "/example-org",
		"-project", "example-project",
		"-repository", "example-scratch",
	}, extra...)
	var stdout, stderr bytes.Buffer
	getenv := func(key string) string {
		if key == tokenEnvironment {
			return testToken
		}
		return ""
	}
	code := run(context.Background(), args, getenv, &stdout, &stderr, server.Client())
	return code, stdout.String(), stderr.String()
}

func newTLSServer(t *testing.T, fake *fakeADO) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(fake)
	t.Cleanup(server.Close)
	return server
}

func TestProvisionDryRunCreatesNothing(t *testing.T) {
	t.Parallel()
	fake := newFakeADO()
	server := newTLSServer(t, fake)

	code, stdout, stderr := runProvision(t, server)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if creates := fake.creates(); len(creates) != 0 {
		t.Fatalf("dry run sent creates %v", creates)
	}
	for _, want := range []string{
		"policy minimum reviewers on refs/heads/main: would create",
		"policy status goobers-live/live-write on refs/heads/main: would create",
		"Fixture work item (#4602): would create",
		"ADO_WRITE_REPOSITORY=example-scratch",
		"Dry run: nothing was created",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if !fake.authOK {
		t.Error("a request did not carry the ADO_PAT basic credential")
	}
}

func TestProvisionApplyIsIdempotentAndNeverMutatesExistingObjects(t *testing.T) {
	t.Parallel()
	fake := newFakeADO()
	server := newTLSServer(t, fake)

	code, stdout, stderr := runProvision(t, server, "-apply")
	if code != 0 {
		t.Fatalf("first apply exit %d, stderr %q", code, stderr)
	}
	if got := len(fake.creates()); got != 3 {
		t.Fatalf("first apply sent %d creates (%v), want two policies and one work item", got, fake.creates())
	}
	fixture := strconv.Itoa(fake.fixture)
	if !strings.Contains(stdout, "ADO_PROVIDER_FIXTURE_WORK_ITEM="+fixture) {
		t.Errorf("stdout does not print the fixture variable:\n%s", stdout)
	}

	code, stdout, stderr = runProvision(t, server, "-apply")
	if code != 0 {
		t.Fatalf("second apply exit %d, stderr %q", code, stderr)
	}
	if got := len(fake.creates()); got != 3 {
		t.Fatalf("second apply created again: %v", fake.creates())
	}
	for _, want := range []string{"minimum reviewers on refs/heads/main: present", "live-write on refs/heads/main: present", "present #" + fixture} {
		if !strings.Contains(stdout, want) {
			t.Errorf("second apply stdout lacks %q:\n%s", want, stdout)
		}
	}
	for _, request := range fake.requests {
		method, _, _ := strings.Cut(request, " ")
		if method != http.MethodGet && method != http.MethodPost {
			t.Errorf("provision sent %s; it must never update or delete", request)
		}
	}
}

func TestProvisionCreatesOnlyExactScopedBlockingPolicies(t *testing.T) {
	t.Parallel()
	fake := newFakeADO()
	server := newTLSServer(t, fake)
	if code, _, stderr := runProvision(t, server, "-apply", "-base", "trunk"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if len(fake.policies) != 2 {
		t.Fatalf("created %d policies, want 2", len(fake.policies))
	}
	for _, policy := range fake.policies {
		if policy["isBlocking"] != true || policy["isEnabled"] != true {
			t.Errorf("policy %v is not enabled and blocking", policy)
		}
		scopes := policy["settings"].(map[string]any)["scope"].([]any)
		for _, raw := range scopes {
			scope := raw.(map[string]any)
			if scope["matchKind"] != "Exact" || scope["refName"] != "refs/heads/trunk" || scope["repositoryId"] != testRepoID {
				t.Errorf("policy scope %v is not exactly refs/heads/trunk in the scratch repository", scope)
			}
		}
	}
}

func TestProvisionFailsWhenABlockingPolicyCoversLiveBranches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		scope map[string]any
		fails bool
	}{
		{"prefix refs/heads/", map[string]any{"repositoryId": testRepoID, "refName": "refs/heads/", "matchKind": "Prefix"}, true},
		{"project-wide prefix", map[string]any{"refName": "refs/heads/goobers", "matchKind": "Prefix"}, true},
		{"no ref", map[string]any{"repositoryId": testRepoID}, true},
		{"exact main", map[string]any{"repositoryId": testRepoID, "refName": "refs/heads/main", "matchKind": "Exact"}, false},
		{"other repository", map[string]any{"repositoryId": "other", "refName": "refs/heads/", "matchKind": "Prefix"}, false},
		{"unrelated prefix", map[string]any{"repositoryId": testRepoID, "refName": "refs/heads/release/", "matchKind": "Prefix"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeADO()
			fake.policies = []map[string]any{{
				"id": 7, "isEnabled": true, "isBlocking": true,
				"type":     map[string]any{"id": "type-build", "displayName": "Build"},
				"settings": map[string]any{"scope": []any{tc.scope}},
			}}
			server := newTLSServer(t, fake)
			code, stdout, stderr := runProvision(t, server)
			if tc.fails != (code != 0) {
				t.Fatalf("exit %d (stderr %q), want failure=%v\n%s", code, stderr, tc.fails, stdout)
			}
			if tc.fails && !strings.Contains(stdout, "blocking policy 7 (Build) covers refs/heads/goobers-live/") {
				t.Errorf("stdout does not name the covering policy:\n%s", stdout)
			}
			if len(fake.creates()) != 0 {
				t.Errorf("dry run sent creates %v", fake.creates())
			}
		})
	}
}

func TestProvisionRequiresTokenFromEnvironmentAndNeverPrintsIt(t *testing.T) {
	t.Parallel()
	fake := newFakeADO()
	server := newTLSServer(t, fake)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"provision", "-organization-url", server.URL + "/example-org", "-project", "example-project", "-repository", "example-scratch"},
		func(string) string { return "" }, &stdout, &stderr, server.Client())
	if code == 0 || !strings.Contains(stderr.String(), tokenEnvironment+" is required") {
		t.Fatalf("exit %d, stderr %q; want a missing-token refusal", code, stderr.String())
	}
	if len(fake.requests) != 0 {
		t.Fatalf("sent requests without a token: %v", fake.requests)
	}

	_, out, errOut := runProvision(t, server, "-apply")
	for _, secret := range []string{testToken, basicAuth(testToken)} {
		if strings.Contains(out+errOut, secret) {
			t.Fatal("output contains the credential")
		}
	}
}

func TestProvisionRejectsMissingFlagsAndPlainHTTP(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{},
		{"deprovision"},
		{"provision", "-project", "p", "-repository", "r"},
		{"provision", "-organization-url", "http://ado.example.invalid/org", "-project", "p", "-repository", "r"},
		{"provision", "-organization-url", "https://ado.example.invalid/org", "-project", "p"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, func(string) string { return testToken }, &stdout, &stderr, http.DefaultClient); code != 2 {
			t.Errorf("run(%q) = %d, want usage error 2", args, code)
		}
	}
}
