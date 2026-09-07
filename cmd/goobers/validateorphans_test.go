package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

type orphanTestLister func(providers.ListWorkItemsRequest) ([]providers.WorkItem, error)

func (f orphanTestLister) ListWorkItems(_ context.Context, req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
	return f(req)
}

func TestOrphanRoutingUsesWorkflowAlternativesAndSameRepositorySiblings(t *testing.T) {
	project := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "team", Name: "repo"}
	gaggle := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "local"}, Spec: apiv1.GaggleSpec{Project: project, RequireLabels: []string{"local"}, Siblings: []apiv1.GaggleSibling{
		{Label: "cloud", Project: project, RequireLabels: []string{"cloud"}},
		{Label: "unrelated", Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "other", Name: "repo"}},
		{Label: "other-host", Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "team", Name: "repo", BaseURL: "https://other.invalid"}},
	}}}
	claim := apiv1.Task{Name: "claim", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--claim"}}, Inputs: map[string]string{"trustLabel": "approved", "excludeLabels": "parked"}}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{gaggle}, Workflows: []apiv1.Workflow{{ObjectMeta: metav1.ObjectMeta{Name: "implement"}, Spec: apiv1.WorkflowSpec{Gaggle: "local", Tasks: []apiv1.Task{claim}}}}}
	cfg := &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "team", Name: "repo"}}}
	demand := gatherRepoRealityDemand("", "", cfg, set)
	scopes := demand[0].orphans
	if len(scopes) != 2 {
		t.Fatalf("scopes include foreign sibling or omit local: %+v", scopes)
	}
	filters, _, err := compileRoutingScopes(scopes)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		labels []string
		match  bool
	}{{[]string{"approved", "local"}, true}, {[]string{"cloud"}, true}, {[]string{"approved"}, false}, {[]string{"approved", "local", "parked"}, false}} {
		got, err := matchesAnyRoutingScope(tc.labels, filters)
		if err != nil || got != tc.match {
			t.Fatalf("labels %v: got %v/%v", tc.labels, got, err)
		}
	}
	// Explicitly empty replaces, rather than merges, the gaggle partition.
	set.Workflows[0].Spec.Tasks[0].Inputs["requireLabels"] = ""
	local := localBacklogRoutingScopes(gaggle, set.Workflows)
	if !reflect.DeepEqual(local[0].required, []string{"approved"}) {
		t.Fatalf("empty override: %+v", local)
	}
}

func TestOrphanRoutingScanPagesAndDeduplicates(t *testing.T) {
	filters, _, err := compileRoutingScopes([]backlogRoutingScope{{name: "local", required: []string{"local"}}, {name: "cloud", expression: `"cloud" in labels`}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	lister := orphanTestLister(func(req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
		calls++
		if req.State != "open" || !reflect.DeepEqual(req.Labels, []string{providers.LabelReady}) || req.Limit != orphanScanPageSize {
			t.Fatalf("unbounded or incorrectly scoped query: %+v", req)
		}
		if calls == 1 {
			req.PageInfo.HasNext, req.PageInfo.NextCursor = true, "next"
			return []providers.WorkItem{{ID: "1", Labels: []string{providers.LabelReady, "local"}}, {ID: "2", Labels: []string{providers.LabelReady}}}, nil
		}
		if req.Cursor != "next" || req.Page != 2 {
			t.Fatalf("continuation lost: %+v", req)
		}
		return []providers.WorkItem{{ID: "2", Labels: []string{providers.LabelReady}}, {ID: "3", Labels: []string{providers.LabelReady, "cloud"}}, {ID: "4", Labels: []string{providers.LabelReady}}, {ID: "5"}}, nil
	})
	var orphans []string
	err = scanOrphanBacklogItems(context.Background(), lister, providers.RepositoryRef{}, filters, func(item providers.WorkItem) { orphans = append(orphans, item.ID) })
	if err != nil || !reflect.DeepEqual(orphans, []string{"2", "4"}) || calls != 2 {
		t.Fatalf("orphans=%v calls=%d error=%v", orphans, calls, err)
	}
}

func TestOrphanRoutingScanBoundAndFailures(t *testing.T) {
	for _, mode := range []string{"bound", "repeat", "error", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			lister := orphanTestLister(func(req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
				calls++
				switch mode {
				case "error":
					return nil, errors.New("unavailable")
				case "oversize":
					return make([]providers.WorkItem, orphanScanPageSize+1), nil
				case "repeat":
					req.PageInfo.HasNext, req.PageInfo.NextCursor = true, "same"
				default:
					req.PageInfo.HasNext, req.PageInfo.NextCursor = true, fmt.Sprint(calls)
				}
				return nil, nil
			})
			err := scanOrphanBacklogItems(context.Background(), lister, providers.RepositoryRef{}, nil, func(providers.WorkItem) { t.Fatal("unexpected orphan") })
			if err == nil || calls > orphanScanPages {
				t.Fatalf("scan unbounded or falsely complete: calls=%d error=%v", calls, err)
			}
			if mode == "bound" && calls != orphanScanPages {
				t.Fatalf("premature truncation: %d", calls)
			}
		})
	}
}

func TestOrphanRoutingDiagnosticsNameIssueAndRoutes(t *testing.T) {
	original := validateRealityLister
	t.Cleanup(func() { validateRealityLister = original })
	validateRealityLister = func(string) repoWorkItemLister {
		return orphanTestLister(func(providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
			return []providers.WorkItem{{ID: "42", Labels: []string{providers.LabelReady}}}, nil
		})
	}
	var out bytes.Buffer
	collector := &diagnosticCollector{}
	warnOrphanBacklogItems("team/repo", instance.RepoRef{Owner: "team", Name: "repo"}, "", []backlogRoutingScope{{name: "local", required: []string{"local"}}, {name: "cloud", required: []string{"cloud"}}}, &out, collector)
	if len(collector.findings) != 1 || collector.findings[0].Code != "SIB002" || collector.findings[0].Severity != "warning" {
		t.Fatalf("JSON diagnostics missing: %+v", collector.findings)
	}
	for _, want := range []string{"SIB002", "#42", "local", "cloud", "does not establish claim eligibility"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
	out.Reset()
	warnOrphanBacklogItems("team/repo", instance.RepoRef{}, "", []backlogRoutingScope{{name: "bad", expression: "invalid("}}, &out)
	if !strings.Contains(out.String(), "SIB003") || strings.Contains(out.String(), "SIB002") {
		t.Fatalf("invalid filter caused an orphan claim: %s", out.String())
	}
}

func TestOrphanRoutingGitHubRawPRPageDoesNotHideNextIssue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/team/repo/issues" || r.URL.Query().Get("labels") != providers.LabelReady || r.URL.Query().Get("state") != "open" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("page") == "1" {
			rows := make([]map[string]any, orphanScanPageSize)
			for i := range rows {
				rows[i] = map[string]any{"number": i + 1, "pull_request": map[string]any{"url": "pr"}}
			}
			_ = json.NewEncoder(w).Encode(rows)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 101, "state": "open", "labels": []map[string]string{{"name": providers.LabelReady}}}})
	}))
	defer server.Close()
	provider := providers.NewGitHubProvider("test")
	provider.BaseURL = server.URL
	var got []string
	err := scanOrphanBacklogItems(context.Background(), provider, providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}, nil, func(item providers.WorkItem) { got = append(got, item.ID) })
	if err != nil || !reflect.DeepEqual(got, []string{"101"}) {
		t.Fatalf("raw PR page hid orphan: %v, %v", got, err)
	}
}
