package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// adoAncestryStageServer fronts adoIssueContextServer: originating work item
// 945 (a Task) links its parent Feature 900 through Hierarchy-Reverse; 900's
// parent is the custom-process "Initiative" 800, whose parent 700 is deleted
// (the batch omits it). Every workitemsbatch call's ids are recorded.
type adoAncestryStageServer struct {
	mu      sync.Mutex
	batches [][]int
}

func (s *adoAncestryStageServer) start(t *testing.T, repo providers.RepositoryRef, description string) *httptest.Server {
	t.Helper()
	inner := adoIssueContextServer(t, repo, "active", "main", description)
	t.Cleanup(inner.Close)
	target, err := url.Parse(inner.URL)
	if err != nil {
		t.Fatal(err)
	}
	project := "/" + repo.Owner + "/" + repo.Project
	mux := http.NewServeMux()
	mux.HandleFunc(project+"/_apis/wit/workitems/945", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, ancestryStageItem(945, repo.Project, "Task", 900, map[string]any{"System.Description": "## Acceptance criteria\n\n- Include this body."}))
	})
	mux.HandleFunc(project+"/_apis/wit/workitemsbatch", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IDs []int `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode workitemsbatch: %v", err)
		}
		s.mu.Lock()
		s.batches = append(s.batches, body.IDs)
		s.mu.Unlock()
		items := map[int]any{
			900: ancestryStageItem(900, repo.Project, "Feature", 800, map[string]any{
				"System.Description":                       "<p>Ship bounded ancestry.</p>",
				"Microsoft.VSTS.Common.AcceptanceCriteria": "<ul><li>Parents are bounded.</li></ul>",
			}),
			800: ancestryStageItem(800, repo.Project, "Initiative", 700, map[string]any{"System.Description": "Initiative intent " + strings.Repeat("x", 64)}),
		}
		values := make([]any, 0, len(body.IDs))
		for _, id := range body.IDs {
			values = append(values, items[id])
		}
		writeJSONResp(t, w, map[string]any{"count": len(values), "value": values})
	})
	mux.Handle("/", httputil.NewSingleHostReverseProxy(target))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func ancestryStageItem(id int, project, itemType string, parent int, fields map[string]any) map[string]any {
	all := map[string]any{
		"System.TeamProject":  project,
		"System.WorkItemType": itemType,
		"System.Title":        itemType + " " + strconv.Itoa(id),
		"System.State":        "Active",
	}
	for name, value := range fields {
		all[name] = value
	}
	return map[string]any{
		"id": id, "rev": 1, "url": "https://ado.example/_apis/wit/workItems/" + strconv.Itoa(id), "fields": all,
		"relations": []map[string]any{{"rel": "System.LinkTypes.Hierarchy-Reverse", "url": "https://ado.example/_apis/wit/workItems/" + strconv.Itoa(parent)}},
	}
}

func runADOAncestryStage(t *testing.T, runID string, inputs map[string]string) (*adoAncestryStageServer, string, int, string) {
	t.Helper()
	root, repo := adoReviewThreadsStageFixture(t, runID)
	seedRemediationBriefRun(t, root, runID, issueContextBrief())
	fake := &adoAncestryStageServer{}
	server := fake.start(t, repo, "Fixes #945")
	routeADOIssueContextProviders(t, server.URL)
	for name, value := range inputs {
		t.Setenv("GOOBERS_INPUT_"+strings.ToUpper(name), value)
	}
	dir := t.TempDir()
	t.Chdir(dir)
	code, _, stderr := runArgs(t, "gather-issue-context", root)
	return fake, dir, code, stderr
}

// TestGatherIssueContextADOAncestryIsBoundedAndSerialized: with traversal
// enabled the brief carries the Feature and the custom Initiative, each
// with its type, state and intent fields, bounded in size; the deleted 700 is
// an explicit omission; each level is one workitemsbatch call; and the
// written brief validates against remediation-brief v3.
func TestGatherIssueContextADOAncestryIsBoundedAndSerialized(t *testing.T) {
	fake, dir, code, stderr := runADOAncestryStage(t, "ado-issue-ancestry", map[string]string{
		"parentTraversal": "true", "parentMaxFieldBytes": "32",
	})
	if code != 0 {
		t.Fatalf("gather-issue-context: code = %d, stderr = %q", code, stderr)
	}
	if want := [][]int{{900}, {800}, {700}}; !reflect.DeepEqual(fake.batches, want) {
		t.Fatalf("workitemsbatch calls = %v, want one per level %v", fake.batches, want)
	}
	got := readIssueContextResult(t, dir)
	if got.GatherIssueContext == nil || len(got.GatherIssueContext.Issues) != 1 {
		t.Fatalf("issue context = %#v, want the originating work item kept", got.GatherIssueContext)
	}
	ancestry := got.GatherIssueContext.Ancestry
	if ancestry == nil {
		t.Fatal("ancestry is absent with parentTraversal enabled")
	}
	if ancestry.Status != "incomplete" || ancestry.MaxDepth != 3 || ancestry.MaxItems != 10 || ancestry.CrossProject != "deny" {
		t.Fatalf("ancestry header = %+v", ancestry)
	}
	if len(ancestry.Items) != 2 {
		t.Fatalf("ancestry items = %+v, want Feature and Initiative", ancestry.Items)
	}
	feature, initiative := ancestry.Items[0], ancestry.Items[1]
	if feature.ID != "900" || feature.Type != "Feature" || feature.Depth != 1 ||
		!reflect.DeepEqual(feature.ParentOf, []string{"ado:" + feature.Project + ":945"}) || feature.QualifiedID != "ado:"+feature.Project+":900" {
		t.Fatalf("feature = %+v", feature)
	}
	if len(feature.Fields) != 2 || feature.Fields[0].Value != "<p>Ship bounded ancestry.</p>" || feature.Fields[1].Name != "Microsoft.VSTS.Common.AcceptanceCriteria" {
		t.Fatalf("feature fields = %+v, want description and acceptance criteria", feature.Fields)
	}
	if initiative.Type != "Initiative" || initiative.Depth != 2 || len(initiative.Fields) != 1 || !initiative.Fields[0].Truncated || len(initiative.Fields[0].Value) != 32 {
		t.Fatalf("initiative = %+v, want the custom type with its description cut to 32 bytes", initiative)
	}
	want := []apiv1.RemediationAncestryOmission{{Child: initiative.QualifiedID, Parent: "700", Depth: 3, Reason: "not-found"}}
	if !reflect.DeepEqual(ancestry.Omissions, want) {
		t.Fatalf("omissions = %+v, want %+v", ancestry.Omissions, want)
	}
	if got.Integrity != apiv1.IntegrityUnapproved {
		t.Fatalf("brief integrity = %q, want unapproved (parents are provider-authored)", got.Integrity)
	}
}

// TestGatherIssueContextAncestryDisabledByDefault: without parentTraversal
// the stage reads no parents and the brief has no ancestry key at all, so
// existing consumers see exactly the output they always did.
func TestGatherIssueContextAncestryDisabledByDefault(t *testing.T) {
	fake, dir, code, stderr := runADOAncestryStage(t, "ado-issue-ancestry-off", nil)
	if code != 0 {
		t.Fatalf("gather-issue-context: code = %d, stderr = %q", code, stderr)
	}
	if len(fake.batches) != 0 {
		t.Fatalf("workitemsbatch calls = %v, want none with traversal off", fake.batches)
	}
	data, err := os.ReadFile(filepath.Join(dir, remediationBriefResultFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"ancestry"`) {
		t.Fatalf("brief carries an ancestry key with traversal disabled:\n%s", data)
	}
}

func TestGatherIssueContextRejectsInvalidAncestryInputs(t *testing.T) {
	for name, inputs := range map[string]map[string]string{
		"depth over ceiling":  {"parentTraversal": "true", "parentMaxDepth": "11"},
		"zero items":          {"parentTraversal": "true", "parentMaxItems": "0"},
		"unknown policy":      {"parentTraversal": "true", "parentCrossProject": "sometimes"},
		"non-boolean enabled": {"parentTraversal": "yes please"},
	} {
		t.Run(name, func(t *testing.T) {
			fake, _, code, stderr := runADOAncestryStage(t, "ado-issue-ancestry-bad", inputs)
			if code != 2 || len(fake.batches) != 0 {
				t.Fatalf("code = %d after %d batch calls, stderr = %q; want usage error before any read", code, len(fake.batches), stderr)
			}
		})
	}
}

// plainIssueReader reads issues but has no parent relation reader.
type plainIssueReader struct{}

func (plainIssueReader) GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error) {
	return providers.WorkItem{}, nil
}

func TestGatherIssueAncestryUnsupportedProviderIsExplicit(t *testing.T) {
	cfg := issueAncestryConfig{enabled: true, options: providers.AncestryOptions{MaxDepth: 3, MaxItems: 10, CrossProject: "deny"}}
	got, err := gatherIssueAncestry(context.Background(), plainIssueReader{}, providers.RepositoryRef{Provider: providers.ProviderGitea},
		[]providers.WorkItem{{ID: "1"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Status != "unsupported" || got.Provider != "gitea" || got.Items == nil || got.Omissions == nil {
		t.Fatalf("ancestry = %+v, want an explicit unsupported result with empty lists", got)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"items":[]`) {
		t.Fatalf("unsupported ancestry = %s, want an explicit empty items list", data)
	}
}
