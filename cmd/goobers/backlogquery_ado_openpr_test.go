package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

type fakeADOOpenPRReader struct {
	prs     []providers.PullRequestSummary
	refs    map[string]providers.PullRequestReferences
	refsErr error
}

func (f fakeADOOpenPRReader) ListPullRequests(context.Context, providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error) {
	return f.prs, nil
}

func (f fakeADOOpenPRReader) PullRequestReferences(_ context.Context, _ providers.RepositoryRef, pullID string) (providers.PullRequestReferences, error) {
	if f.refsErr != nil {
		return providers.PullRequestReferences{}, f.refsErr
	}
	return f.refs[pullID], nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func TestADOReferencedBacklogItemsByTopology(t *testing.T) {
	adoRepo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "proj", Name: "repo"}
	githubBacklog := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "backlog"}
	reader := fakeADOOpenPRReader{
		prs: []providers.PullRequestSummary{{ID: "7"}, {ID: "8"}},
		refs: map[string]providers.PullRequestReferences{
			"7": {Body: "Implements #42\n\nsee also #99\n\nFixes https://github.com/acme/backlog/issues/5", WorkItemIDs: []string{"43"}},
			"8": {Body: "Closes #44"},
		},
	}
	for _, tc := range []struct {
		name    string
		backlog providers.RepositoryRef
		want    []string
	}{
		{name: "ado backlog", backlog: adoRepo, want: []string{"42", "43", "44"}},
		{name: "topology b", backlog: githubBacklog, want: []string{"5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := adoReferencedBacklogItems(context.Background(), reader, adoRepo, tc.backlog)
			if err != nil {
				t.Fatalf("adoReferencedBacklogItems: %v", err)
			}
			if keys := sortedKeys(got); !reflect.DeepEqual(keys, tc.want) {
				t.Fatalf("referenced items = %v, want %v", keys, tc.want)
			}
		})
	}

	reader.refsErr = errors.New("boom")
	if _, err := adoReferencedBacklogItems(context.Background(), reader, adoRepo, adoRepo); err == nil || !strings.Contains(err.Error(), "pull request 7") {
		t.Fatalf("reference read error = %v, want failure naming pull request 7", err)
	}
}

// adoOpenPRBackstopServer serves an ADO backlog of work items 42, 43 and 44
// and, unless failPRs is set, one active goober pull request that references
// 42 by text and links 43 natively, plus one human pull request outside the
// run-branch namespace, which is never read.
func adoOpenPRBackstopServer(t *testing.T, failPRs bool) *httptest.Server {
	t.Helper()
	const selfIdentityID = "00000000-0000-0000-0000-000000006919"
	var mu sync.Mutex
	tags := map[string]string{"42": "goobers:approved", "43": "goobers:approved", "44": "goobers:approved"}
	comments := map[string][]map[string]any{}
	item := func(id string) map[string]any {
		number, _ := strconv.Atoi(id)
		return map[string]any{"id": number, "rev": 1, "fields": map[string]any{
			"System.WorkItemType": "Issue",
			"System.Title":        "ADO item " + id,
			"System.State":        "Active",
			"System.Tags":         tags[id],
		}}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/backlog/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		writeADOJSON(t, w, map[string]any{"workItems": []map[string]int{{"id": 42}, {"id": 43}, {"id": 44}}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workitemtypes/Issue/states", func(w http.ResponseWriter, r *http.Request) {
		writeADOJSON(t, w, map[string]any{"value": []map[string]string{{"name": "Active", "category": "InProgress"}}})
	})
	mux.HandleFunc("/org/_apis/connectionData", func(w http.ResponseWriter, r *http.Request) {
		writeADOJSON(t, w, map[string]any{"authenticatedUser": map[string]any{"id": selfIdentityID, "providerDisplayName": "Goobers Bot"}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workitemsbatch", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		writeADOJSON(t, w, map[string]any{"value": []map[string]any{item("42"), item("43"), item("44")}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workItems/{id}/updates", func(w http.ResponseWriter, r *http.Request) {
		writeADOJSON(t, w, map[string]any{"count": 0, "value": []map[string]any{}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workItems/{id}/comments", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		id := r.PathValue("id")
		if r.Method == http.MethodPost {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode comment: %v", err)
			}
			comment := map[string]any{
				"id":        len(comments[id]) + 1,
				"text":      body["text"],
				"createdBy": map[string]string{"id": selfIdentityID, "displayName": "Goobers Bot"},
			}
			comments[id] = append(comments[id], comment)
			writeADOJSON(t, w, comment)
			return
		}
		writeADOJSON(t, w, map[string]any{"comments": comments[id]})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workitems/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		id := r.PathValue("id")
		if r.Method == http.MethodPatch {
			var patch []map[string]any
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Errorf("decode work item patch: %v", err)
			}
			for _, operation := range patch {
				if operation["path"] == "/fields/System.Tags" {
					tags[id], _ = operation["value"].(string)
				}
			}
		}
		writeADOJSON(t, w, item(id))
	})
	mux.HandleFunc("/org/backlog/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		if failPRs {
			http.Error(w, "unavailable", http.StatusBadRequest)
			return
		}
		writeADOJSON(t, w, map[string]any{"value": []map[string]any{
			{"pullRequestId": 7, "status": "active", "sourceRefName": "refs/heads/goobers/run-1", "targetRefName": "refs/heads/main"},
			{"pullRequestId": 9, "status": "active", "sourceRefName": "refs/heads/human/topic", "targetRefName": "refs/heads/main"},
		}})
	})
	mux.HandleFunc("/org/backlog/_apis/git/repositories/repo/pullrequests/7", func(w http.ResponseWriter, r *http.Request) {
		writeADOJSON(t, w, map[string]any{"pullRequestId": 7, "status": "active", "description": "Implementation.\n\nFixes #42"})
	})
	mux.HandleFunc("/org/backlog/_apis/git/repositories/repo/pullrequests/7/workitems", func(w http.ResponseWriter, r *http.Request) {
		writeADOJSON(t, w, map[string]any{"value": []map[string]string{{"id": "43"}}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected ADO request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	previous := stageProviderFactories[providers.ProviderADO]
	t.Cleanup(func() { stageProviderFactories[providers.ProviderADO] = previous })
	stageProviderFactories[providers.ProviderADO] = func(cfg stageProviderConfig) (providers.Provider, error) {
		if cfg.capability != capability.GitHubPRWrite || cfg.token != "pr-token" {
			t.Errorf("ADO PR provider built with capability %q token %q, want github:pr:write pr-token", cfg.capability, cfg.token)
		}
		return providers.NewADOProvider(cfg.repo.Owner, cfg.repo.Project, cfg.token, func(p *providers.ADOProvider) {
			p.BaseURL = server.URL
		}), nil
	}
	return server
}

func runADOOpenPRBackstopClaim(t *testing.T, server *httptest.Server) (int, string, string) {
	t.Helper()
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "backlog", Name: "repo"}
	root := initDemo(t)
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv(executor.RunIDEnvVar, "ado-openpr-run")
	t.Setenv(executor.GaggleEnvVar, "example")
	t.Setenv(executor.WorkflowEnvVar, "implementation")
	t.Chdir(t.TempDir())
	var stdout, stderr bytes.Buffer
	env := backlogQueryEnv{
		root:        root,
		layout:      layoutFor(root),
		repo:        repo,
		backlogRepo: repo,
		issueProvider: providers.NewADOProvider("org", "backlog", "token", func(p *providers.ADOProvider) {
			p.BaseURL = server.URL
		}),
		stdout: &stdout,
		stderr: &stderr,
	}
	code := runBacklogQueryMode(backlogQueryModeClaim, env, nil)
	return code, stdout.String(), stderr.String()
}

// TestADOBacklogQueryOpenPRBackstop pins #6919: on Azure DevOps code a work
// item an active goober pull request references, by "Fixes #N" or by a native
// work-item link, is not claimed again.
func TestADOBacklogQueryOpenPRBackstop(t *testing.T) {
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubPRWrite)), "pr-token")
	server := adoOpenPRBackstopServer(t, false)
	code, stdout, stderr := runADOOpenPRBackstopClaim(t, server)
	if code != 0 || !strings.Contains(stdout, "claimed 44") {
		t.Fatalf("code=%d stdout=%q stderr=%q, want item 44 claimed past the items with an open pull request", code, stdout, stderr)
	}
}

func TestADOBacklogQueryOpenPRBackstopNeedsPRCredential(t *testing.T) {
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubPRWrite)), "")
	server := adoOpenPRBackstopServer(t, false)
	code, stdout, stderr := runADOOpenPRBackstopClaim(t, server)
	if code != 0 || !strings.Contains(stdout, "claimed 42") {
		t.Fatalf("code=%d stdout=%q stderr=%q, want label-only eligibility (item 42) without a pull-request credential", code, stdout, stderr)
	}
}

func TestADOBacklogQueryOpenPRBackstopFailsClosed(t *testing.T) {
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubPRWrite)), "pr-token")
	server := adoOpenPRBackstopServer(t, true)
	code, stdout, stderr := runADOOpenPRBackstopClaim(t, server)
	if code == 0 || !strings.Contains(stderr, "list open pull requests") || strings.Contains(stdout, "claimed") {
		t.Fatalf("code=%d stdout=%q stderr=%q, want a failed list open pull requests stage that claims nothing", code, stdout, stderr)
	}
}
