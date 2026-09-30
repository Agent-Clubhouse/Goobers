package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// adoScopeBoard is a minimal Azure Boards fake for #6098: every work item
// lives in the one project, WIQL returns all of them (the scope must still
// hold when the server-side CONTAINS is a loose match), and a claim's tag
// patch is applied so the claim marker is observable.
type adoScopeBoard struct {
	mu       sync.Mutex
	tags     map[int]string
	comments map[int][]map[string]any
	queries  []string
}

const adoScopeSelfID = "00000000-0000-0000-0000-000000006098"

func newADOScopeBoard(t *testing.T, tags map[int]string) (*adoScopeBoard, *providers.ADOProvider) {
	t.Helper()
	board := &adoScopeBoard{tags: tags, comments: map[int][]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/backlog/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		board.mu.Lock()
		board.queries = append(board.queries, body["query"])
		board.mu.Unlock()
		refs := []map[string]int{}
		for _, id := range board.ids() {
			refs = append(refs, map[string]int{"id": id})
		}
		writeADOJSON(t, w, map[string]any{"workItems": refs})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workitemtypes/Issue/states", func(w http.ResponseWriter, r *http.Request) {
		writeADOJSON(t, w, map[string]any{"value": []map[string]string{{"name": "Active", "category": "InProgress"}}})
	})
	mux.HandleFunc("/org/_apis/connectionData", func(w http.ResponseWriter, r *http.Request) {
		writeADOJSON(t, w, map[string]any{"authenticatedUser": map[string]any{
			"id": adoScopeSelfID, "providerDisplayName": "Goobers Bot",
		}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workitemsbatch", func(w http.ResponseWriter, r *http.Request) {
		var items []map[string]any
		for _, id := range board.ids() {
			items = append(items, board.item(id))
		}
		writeADOJSON(t, w, map[string]any{"value": items})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/", func(w http.ResponseWriter, r *http.Request) {
		board.serveItem(t, w, r)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	provider := providers.NewADOProvider("org", "backlog", "token", func(p *providers.ADOProvider) {
		p.BaseURL = server.URL
	})
	return board, provider
}

func (b *adoScopeBoard) ids() []int {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]int, 0, len(b.tags))
	for id := range b.tags {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func (b *adoScopeBoard) item(id int) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]any{"id": id, "rev": 1, "fields": map[string]any{
		"System.WorkItemType": "Issue",
		"System.Title":        "item " + strconv.Itoa(id),
		"System.State":        "Active",
		"System.Tags":         b.tags[id],
	}}
}

// serveItem answers /workitems/{id}, /workItems/{id}/comments and
// /workItems/{id}/updates.
func (b *adoScopeBoard) serveItem(t *testing.T, w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/org/backlog/_apis/wit/"), "/")
	if len(parts) < 2 || !strings.EqualFold(parts[0], "workitems") {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.Atoi(parts[1])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case len(parts) == 3 && parts[2] == "comments":
		writeADOJSON(t, w, b.serveComments(t, id, r))
	case len(parts) == 3 && parts[2] == "updates":
		// The ready tag's add event, so a claim can measure ready age.
		writeADOJSON(t, w, map[string]any{"count": 1, "value": []map[string]any{{"id": 1, "fields": map[string]any{
			"System.Tags":        map[string]any{"oldValue": "goobers:approved", "newValue": b.item(id)["fields"].(map[string]any)["System.Tags"]},
			"System.ChangedDate": map[string]any{"newValue": "2026-09-01T00:00:00Z"},
		}}}})
	case len(parts) == 2:
		b.patchTags(t, id, r)
		writeADOJSON(t, w, b.item(id))
	default:
		http.NotFound(w, r)
	}
}

func (b *adoScopeBoard) serveComments(t *testing.T, id int, r *http.Request) any {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Method != http.MethodPost {
		return map[string]any{"comments": b.comments[id]}
	}
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode comment: %v", err)
	}
	comment := map[string]any{
		"id": len(b.comments[id]) + 1, "text": body["text"],
		"createdBy": map[string]string{"id": adoScopeSelfID, "displayName": "Goobers Bot"},
	}
	b.comments[id] = append(b.comments[id], comment)
	return comment
}

func (b *adoScopeBoard) patchTags(t *testing.T, id int, r *http.Request) {
	if r.Method != http.MethodPatch {
		return
	}
	var patch []map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		t.Errorf("decode work item patch: %v", err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, operation := range patch {
		if operation["path"] == "/fields/System.Tags" {
			b.tags[id], _ = operation["value"].(string)
		}
	}
}

func (b *adoScopeBoard) wiql() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.queries, "\n")
}

// adoScopedGaggleFixture turns the demo gaggle into an Azure DevOps gaggle
// whose backlog is the project "backlog" and whose spec.backlog.labels is
// scope (none when empty).
func adoScopedGaggleFixture(t *testing.T, root string, scope ...string) {
	t.Helper()
	adoBacklogProjectFixture(t, root, "example", "backlog", "backlog")
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	raw, err := os.ReadFile(gagglePath)
	if err != nil {
		t.Fatalf("read gaggle: %v", err)
	}
	labels := ""
	if len(scope) > 0 {
		labels = "    labels:\n"
		for _, label := range scope {
			labels += "      - " + label + "\n"
		}
	}
	updated := strings.Replace(string(raw), "    labels:\n      - goobers\n", labels, 1)
	if updated == string(raw) {
		t.Fatalf("starter gaggle has no backlog.labels block to replace:\n%s", raw)
	}
	if err := os.WriteFile(gagglePath, []byte(updated), 0o644); err != nil {
		t.Fatalf("write gaggle: %v", err)
	}
}

func runADOScopedBacklogQuery(t *testing.T, root string, provider *providers.ADOProvider, mode backlogQueryMode) (string, string) {
	t.Helper()
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "backlog", Name: "web"}
	var stdout, stderr bytes.Buffer
	env := backlogQueryEnv{
		root: root, layout: layoutFor(root), repo: repo, backlogRepo: repo,
		issueProvider: provider, stdout: &stdout, stderr: &stderr,
	}
	if code := runBacklogQueryMode(mode, env, nil); code != 0 {
		t.Fatalf("backlog-query mode %d: code=%d stdout=%q stderr=%q", mode, code, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

func setADOScopeStageEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", providers.LabelReady)
	t.Setenv(executor.RunIDEnvVar, "run-6098")
	t.Setenv(executor.GaggleEnvVar, "example")
	t.Setenv(executor.WorkflowEnvVar, "implementation")
	t.Chdir(t.TempDir())
}

// TestADOBacklogQueryScopedToGaggleBacklogLabels is #6098: on Azure DevOps,
// where Boards are project-wide, a gaggle with spec.backlog.labels [g1] must
// neither list nor claim an approved+ready item that lacks g1 (it belongs to
// another gaggle on the same project), and must claim one that carries it.
func TestADOBacklogQueryScopedToGaggleBacklogLabels(t *testing.T) {
	root := initDemo(t)
	adoScopedGaggleFixture(t, root, "g1")
	setADOScopeStageEnv(t)
	board, provider := newADOScopeBoard(t, map[int]string{
		41: "goobers:approved; goobers:ready",
		42: "goobers:approved; goobers:ready; other-gaggle",
		43: "goobers:approved; goobers:ready; g1",
	})

	listed, _ := runADOScopedBacklogQuery(t, root, provider, backlogQueryModeReadOnly)
	if strings.Contains(listed, "41\t") || strings.Contains(listed, "42\t") || !strings.Contains(listed, "43\titem 43") {
		t.Fatalf("read-only output = %q, want only the g1-tagged item 43", listed)
	}
	if !strings.Contains(board.wiql(), "[System.Tags] CONTAINS 'g1'") {
		t.Fatalf("WIQL did not filter on the gaggle scope server-side:\n%s", board.wiql())
	}

	claimed, _ := runADOScopedBacklogQuery(t, root, provider, backlogQueryModeClaim)
	if !strings.Contains(claimed, "claimed 43") {
		t.Fatalf("claim output = %q, want claimed 43", claimed)
	}
	for _, id := range []int{41, 42} {
		if tags := board.tags[id]; strings.Contains(tags, providers.LabelClaimed) {
			t.Fatalf("item %d outside the gaggle scope was claimed: tags %q", id, tags)
		}
	}
}

// TestADOBacklogQueryScopeComposesWithLabelPredicate pins that the gaggle
// scope is ANDed with a workflow's own labelPredicate: only the item that
// carries the scope label AND satisfies the predicate is claimable.
func TestADOBacklogQueryScopeComposesWithLabelPredicate(t *testing.T) {
	root := initDemo(t)
	adoScopedGaggleFixture(t, root, "g1")
	setADOScopeStageEnv(t)
	t.Setenv("GOOBERS_INPUT_LABELPREDICATE", `"wanted" in labels`)
	board, provider := newADOScopeBoard(t, map[int]string{
		41: "goobers:approved; goobers:ready; wanted",
		42: "goobers:approved; goobers:ready; g1",
		43: "goobers:approved; goobers:ready; g1; wanted",
	})

	claimed, _ := runADOScopedBacklogQuery(t, root, provider, backlogQueryModeClaim)
	if !strings.Contains(claimed, "claimed 43") {
		t.Fatalf("claim output = %q, want claimed 43 (g1 AND wanted)", claimed)
	}
	for _, id := range []int{41, 42} {
		if tags := board.tags[id]; strings.Contains(tags, providers.LabelClaimed) {
			t.Fatalf("item %d failing scope or predicate was claimed: tags %q", id, tags)
		}
	}
}

// TestADOBacklogQueryUnscopedGaggleWarns keeps today's behavior for an ADO
// gaggle that declares no backlog.labels — it still claims — and warns that
// its selection spans the whole project.
func TestADOBacklogQueryUnscopedGaggleWarns(t *testing.T) {
	root := initDemo(t)
	adoScopedGaggleFixture(t, root)
	setADOScopeStageEnv(t)
	_, provider := newADOScopeBoard(t, map[int]string{41: "goobers:approved; goobers:ready"})

	claimed, stderr := runADOScopedBacklogQuery(t, root, provider, backlogQueryModeClaim)
	if !strings.Contains(claimed, "claimed 41") {
		t.Fatalf("claim output = %q, want claimed 41", claimed)
	}
	if !strings.Contains(stderr, "declares no spec.backlog.labels") {
		t.Fatalf("stderr = %q, want the unscoped-ADO-gaggle warning", stderr)
	}
}

// TestBacklogQueryRequireLabelsUnchangedOffADO pins that the #6098 scope is
// ADO-only: a GitHub or Gitea backlog — including a GitHub backlog for Azure
// DevOps code (topology (b)) — keeps exactly the stage's requireLabels input
// even though its gaggle declares spec.backlog.labels, while an ADO backlog
// gains them (deduplicated ignoring case, as ADO tags are).
func TestBacklogQueryRequireLabelsUnchangedOffADO(t *testing.T) {
	root := initDemo(t) // the starter gaggle's GitHub backlog declares labels [goobers]
	t.Setenv(executor.GaggleEnvVar, "example")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	github := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	gitea := providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: "your-org", Name: "your-repo"}
	adoCode := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "code", Name: "web"}
	for name, env := range map[string]backlogQueryEnv{
		"github":      {root: root, repo: github, backlogRepo: github},
		"gitea":       {root: root, repo: gitea, backlogRepo: gitea},
		"topology-b":  {root: root, repo: adoCode, backlogRepo: github},
		"ado-no-conf": {root: root, repo: adoCode, backlogRepo: adoCode}, // starter gaggle's backlog is GitHub
	} {
		env.stderr = &bytes.Buffer{}
		if got := backlogQueryRequireLabels(env); !reflect.DeepEqual(got, []string{"goobers:ready"}) {
			t.Errorf("%s: requireLabels = %q, want the input unchanged", name, got)
		}
	}

	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{Spec: apiv1.GaggleSpec{Backlog: apiv1.BacklogRef{
		Provider: apiv1.ProviderADO, Project: "backlog", Labels: []string{"G1", "goobers:READY"},
	}}}}}
	set.Gaggles[0].Name = "example"
	labels, ado := gaggleADOBacklogLabels(set, "example")
	if !ado {
		t.Fatalf("gaggleADOBacklogLabels reported a non-ADO backlog")
	}
	if got := appendMissingLabels([]string{"goobers:ready"}, labels); !reflect.DeepEqual(got, []string{"goobers:ready", "G1"}) {
		t.Fatalf("ADO scoped requireLabels = %q, want [goobers:ready G1]", got)
	}
}
