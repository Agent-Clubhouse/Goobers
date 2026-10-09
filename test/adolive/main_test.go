package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	testToken     = "test-token-value"
	testRepoID    = "11111111-2222-3333-4444-555555555555"
	reviewersID   = "type-reviewers"
	statusTypeID  = "type-status"
	strategyID    = "type-merge-strategy"
	testQueueID   = 9
	requirementTy = "User Story"
)

// fakeItem is one work item the fake holds.
type fakeItem struct {
	ID        int
	Type      string
	Rev       int
	Fields    map[string]any
	Relations []map[string]any
}

// fakeADO serves the handful of Azure DevOps endpoints provision touches and
// records every request it receives.
type fakeADO struct {
	mu          sync.Mutex
	requests    []string
	policies    []map[string]any
	items       []*fakeItem
	definitions []map[string]any
	nextID      int
	authOK      bool
}

func newFakeADO() *fakeADO { return &fakeADO{nextID: 100, authOK: true} }

var wiqlClause = regexp.MustCompile(`\[System\.Title\] = '([^']*)' AND \[System\.Tags\] CONTAINS '([^']*)'`)

func (f *fakeADO) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != basicAuth(testToken) {
		f.authOK = false
	}
	body, _ := io.ReadAll(r.Body)
	const project = "/example-org/example-project/_apis/"
	path := strings.TrimPrefix(r.URL.Path, project)
	switch {
	case r.Method == http.MethodGet && path == "git/repositories/example-scratch":
		writeJSON(w, map[string]string{"id": testRepoID, "name": "example-scratch"})
	case r.Method == http.MethodGet && path == "policy/types":
		writeJSON(w, map[string]any{"value": []map[string]string{
			{"id": reviewersID, "displayName": "Minimum number of reviewers"},
			{"id": statusTypeID, "displayName": "Status"},
			{"id": strategyID, "displayName": "Require a merge strategy"},
		}})
	case r.Method == http.MethodGet && path == "policy/configurations":
		writeJSON(w, map[string]any{"value": f.policies})
	case r.Method == http.MethodPost && path == "policy/configurations":
		var created map[string]any
		_ = json.Unmarshal(body, &created)
		f.nextID++
		created["id"] = f.nextID
		created["type"] = map[string]any{"id": created["type"].(map[string]any)["id"], "displayName": "created"}
		f.policies = append(f.policies, created)
		writeJSON(w, created)
	case r.Method == http.MethodPost && path == "wit/wiql":
		writeJSON(w, map[string]any{"workItems": f.wiql(body)})
	case r.Method == http.MethodGet && path == "wit/workitemtypecategories/Microsoft.RequirementCategory":
		writeJSON(w, map[string]any{"defaultWorkItemType": map[string]string{"name": requirementTy}})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "wit/workitems/$"):
		writeJSON(w, map[string]int{"id": f.create(strings.TrimPrefix(path, "wit/workitems/$"), body)})
	case strings.HasPrefix(path, "wit/workitems/"):
		f.serveItem(w, r, strings.TrimPrefix(path, "wit/workitems/"), body)
	case r.Method == http.MethodGet && path == "build/definitions":
		var matches []map[string]any
		for _, d := range f.definitions {
			if d["name"] == r.URL.Query().Get("name") {
				matches = append(matches, d)
			}
		}
		writeJSON(w, map[string]any{"value": matches})
	case r.Method == http.MethodPost && path == "build/definitions":
		var created map[string]any
		_ = json.Unmarshal(body, &created)
		f.nextID++
		created["id"] = f.nextID
		f.definitions = append(f.definitions, created)
		writeJSON(w, created)
	case r.Method == http.MethodGet && path == "distributedtask/queues":
		writeJSON(w, map[string]any{"value": []map[string]any{{"id": testQueueID, "name": r.URL.Query().Get("queueName")}}})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

// wiql answers provision's title-and-tag lookups, lowest id first.
func (f *fakeADO) wiql(body []byte) []map[string]int {
	var request struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal(body, &request)
	match := wiqlClause.FindStringSubmatch(request.Query)
	out := []map[string]int{}
	for _, item := range f.items {
		if match != nil && item.Fields["System.Title"] == match[1] && strings.Contains(fmt.Sprint(item.Fields["System.Tags"]), match[2]) {
			out = append(out, map[string]int{"id": item.ID})
		}
	}
	return out
}

// create applies a creating JSON Patch.
func (f *fakeADO) create(itemType string, body []byte) int {
	f.nextID++
	item := &fakeItem{ID: f.nextID, Type: itemType, Rev: 1, Fields: map[string]any{}}
	f.applyPatch(item, body)
	f.items = append(f.items, item)
	return item.ID
}

func (f *fakeADO) applyPatch(item *fakeItem, body []byte) {
	var ops []struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value any    `json:"value"`
	}
	_ = json.Unmarshal(body, &ops)
	for _, op := range ops {
		switch {
		case op.Op == "add" && strings.HasPrefix(op.Path, "/fields/"):
			item.Fields[strings.TrimPrefix(op.Path, "/fields/")] = op.Value
		case op.Op == "add" && op.Path == "/relations/-":
			item.Relations = append(item.Relations, op.Value.(map[string]any))
		}
	}
}

func (f *fakeADO) item(id int) *fakeItem {
	for _, item := range f.items {
		if item.ID == id {
			return item
		}
	}
	return nil
}

func (f *fakeADO) itemTitled(title string) *fakeItem {
	for _, item := range f.items {
		if item.Fields["System.Title"] == title {
			return item
		}
	}
	return nil
}

func (f *fakeADO) serveItem(w http.ResponseWriter, r *http.Request, rawID string, body []byte) {
	id, _ := strconv.Atoi(rawID)
	item := f.item(id)
	if item == nil {
		http.Error(w, "no work item "+rawID, http.StatusNotFound)
		return
	}
	if r.Method == http.MethodPatch {
		f.applyPatch(item, body)
		item.Rev++
	}
	writeJSON(w, map[string]any{"id": item.ID, "rev": item.Rev, "fields": item.Fields, "relations": item.Relations})
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
		"policy merge strategy on refs/heads/main: would create",
		"Fixture work item (#4602): would create",
		"Ancestry parent work item (#6125): would create a Feature",
		"Spec fixture work item (#6125, #6191): would create",
		"ADO_WRITE_REPOSITORY=example-scratch",
		"ADO_LIVE_SPEC_WORK_ITEM=<id printed by a run with -apply>",
		"ADO_LIVE_CI_FAILURE_PIPELINE: not provisioned",
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
	if got := len(fake.creates()); got != 6 {
		t.Fatalf("first apply sent %d creates (%v), want three policies and three work items", got, fake.creates())
	}
	fixture := strconv.Itoa(fake.itemTitled(fixtureTitle).ID)
	spec := strconv.Itoa(fake.itemTitled(specTitle).ID)
	for _, want := range []string{"provider fixture #" + fixture + ": no variable", "ADO_LIVE_SPEC_WORK_ITEM=" + spec} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not print %q:\n%s", want, stdout)
		}
	}

	code, stdout, stderr = runProvision(t, server, "-apply")
	if code != 0 {
		t.Fatalf("second apply exit %d, stderr %q", code, stderr)
	}
	if got := len(fake.creates()); got != 6 {
		t.Fatalf("second apply created again: %v", fake.creates())
	}
	for _, want := range []string{
		"minimum reviewers on refs/heads/main: present", "live-write on refs/heads/main: present",
		"merge strategy on refs/heads/main: present", "present #" + fixture,
		"Spec fixture work item (#6125, #6191): present #" + spec, "parent link to #",
	} {
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
	if len(fake.policies) != 3 {
		t.Fatalf("created %d policies, want 3", len(fake.policies))
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

// TestProvisionSpecFixturesShapeTheReadOnlyLegsInputs pins what the read-only
// conformance leg asserts against: a parent with a description, and a child of
// the project's requirement type with an empty description, acceptance
// criteria and a Hierarchy-Reverse link to that parent.
func TestProvisionSpecFixturesShapeTheReadOnlyLegsInputs(t *testing.T) {
	t.Parallel()
	fake := newFakeADO()
	server := newTLSServer(t, fake)
	if code, _, stderr := runProvision(t, server, "-apply"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	parent, spec := fake.itemTitled(parentTitle), fake.itemTitled(specTitle)
	if parent == nil || spec == nil {
		t.Fatalf("parent %v, spec %v; want both created", parent, spec)
	}
	if parent.Type != "Feature" || parent.Fields["System.Description"] != parentBody || parent.Fields["System.Tags"] != liveFixtureTag {
		t.Errorf("parent = %+v", parent)
	}
	if spec.Type != requirementTy || spec.Fields[acceptanceCriteriaField] != specAcceptanceCriteria || spec.Fields["System.Tags"] != liveFixtureTag {
		t.Errorf("spec = %+v", spec)
	}
	if _, ok := spec.Fields["System.Description"]; ok {
		t.Errorf("spec has a description %q; the leg needs it empty", spec.Fields["System.Description"])
	}
	if len(spec.Relations) != 1 || spec.Relations[0]["rel"] != hierarchyReverse ||
		!strings.HasSuffix(fmt.Sprint(spec.Relations[0]["url"]), "/_apis/wit/workItems/"+strconv.Itoa(parent.ID)) {
		t.Errorf("spec relations = %v, want one Hierarchy-Reverse link to #%d", spec.Relations, parent.ID)
	}
}

// TestProvisionAddsOnlyAMissingSpecParentLink: an existing spec fixture with
// no parent gains the link (the tool's one write to an existing object); one
// linked to another parent is refused, never re-linked.
func TestProvisionAddsOnlyAMissingSpecParentLink(t *testing.T) {
	t.Parallel()
	fake := newFakeADO()
	server := newTLSServer(t, fake)
	if code, _, stderr := runProvision(t, server, "-apply"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	spec := fake.itemTitled(specTitle)
	spec.Relations = nil
	code, stdout, stderr := runProvision(t, server, "-apply")
	if code != 0 || !strings.Contains(stdout, "parent link to #") || !strings.Contains(stdout, ": added") {
		t.Fatalf("exit %d (stderr %q), want the missing link added:\n%s", code, stderr, stdout)
	}
	if len(spec.Relations) != 1 {
		t.Fatalf("spec relations = %v after re-link", spec.Relations)
	}

	spec.Relations[0]["url"] = "https://example.invalid/_apis/wit/workItems/1"
	code, stdout, _ = runProvision(t, server, "-apply")
	if code == 0 || !strings.Contains(stdout, "re-link it by hand") {
		t.Fatalf("exit %d, want a refusal for a foreign parent:\n%s", code, stdout)
	}
	if got := spec.Relations[0]["url"]; got != "https://example.invalid/_apis/wit/workItems/1" {
		t.Fatalf("foreign parent link was rewritten to %v", got)
	}
}

func TestProvisionCIPipelineIsOptInAndIdempotent(t *testing.T) {
	t.Parallel()
	fake := newFakeADO()
	server := newTLSServer(t, fake)
	if code, _, stderr := runProvision(t, server, "-apply"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if len(fake.definitions) != 0 {
		t.Fatalf("created a build definition without -ci-pipeline: %v", fake.definitions)
	}
	for range 2 {
		code, stdout, stderr := runProvision(t, server, "-apply", "-ci-pipeline")
		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if len(fake.definitions) != 1 {
			t.Fatalf("definitions = %v, want exactly one", fake.definitions)
		}
		id := fmt.Sprint(fake.definitions[0]["id"])
		if !strings.Contains(stdout, "ADO_LIVE_CI_FAILURE_PIPELINE="+id) {
			t.Errorf("stdout does not print the pipeline variable:\n%s", stdout)
		}
	}
	definition := fake.definitions[0]
	process := definition["process"].(map[string]any)
	repository := definition["repository"].(map[string]any)
	queue := definition["queue"].(map[string]any)
	if definition["name"] != ciPipelineName || process["yamlFilename"] != ciPipelineYAML || process["type"] != float64(2) ||
		repository["id"] != testRepoID || repository["type"] != "TfsGit" || queue["id"] != float64(testQueueID) {
		t.Errorf("definition = %v", definition)
	}
}
