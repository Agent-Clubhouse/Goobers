package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// adoBacklogProjectFixture points the demo gaggle's backlog at a *different* ADO
// project than the routed code repository, the real ADO topology: branches and
// pull requests land in the code project while the claimed work item lives in
// the backlog project.
func adoBacklogProjectFixture(t *testing.T, root, gaggle, codeProject, backlogProject string) {
	t.Helper()
	gagglePath := filepath.Join(root, "config", "gaggles", gaggle, "gaggle.yaml")
	raw, err := os.ReadFile(gagglePath)
	if err != nil {
		t.Fatalf("read gaggle: %v", err)
	}
	updated := strings.Replace(string(raw),
		"  backlog:\n    provider: github\n    project: your-org/your-repo\n",
		"  backlog:\n    provider: ado\n    project: "+backlogProject+"\n", 1)
	if updated == string(raw) {
		t.Fatalf("starter gaggle did not contain the expected github backlog block:\n%s", raw)
	}
	if err := os.WriteFile(gagglePath, []byte(updated), 0o644); err != nil {
		t.Fatalf("write gaggle: %v", err)
	}

	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Repos = []instance.RepoRef{{
		Provider: "ado",
		Owner:    "acme",
		Project:  codeProject,
		Name:     "web",
		Token:    instance.TokenRef{Env: "ADO_OPEN_PR_STALENESS_PAT"},
	}}
	if err := instance.WriteConfig(layoutFor(root).ConfigFile(), cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("ADO_OPEN_PR_STALENESS_PAT", "pat")
}

// TestOpenPRStalenessChecksBacklogProjectOnADO is #3648: the mid-flight #947
// staleness re-check must read the claimed work item from the *backlog* project,
// not the routed code repository's project. Addressed at the code project the
// read returns not-found, and because the check deliberately fails open that
// silently opens a PR for a work item that was already closed — precisely the
// stale PR #947 exists to prevent.
func TestOpenPRStalenessChecksBacklogProjectOnADO(t *testing.T) {
	const (
		gaggle         = "example"
		codeProject    = "code-project"
		backlogProject = "backlog-project"
		runID          = "run-3648"
	)
	root := initDemo(t)
	adoBacklogProjectFixture(t, root, gaggle, codeProject, backlogProject)
	recordClaimedIssue(t, root, runID, "1456", "Superseded ADO work")

	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Project: codeProject, Name: "web"}
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv("GOOBERS_GAGGLE", gaggle)
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "implementation")

	mux := http.NewServeMux()
	// The work item lives ONLY in the backlog project, and it is closed.
	mux.HandleFunc("/acme/"+backlogProject+"/_apis/wit/workitems/1456", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{
			"id":  1456,
			"rev": 1,
			"fields": map[string]interface{}{
				"System.WorkItemType": "Issue",
				"System.Title":        "Superseded ADO work",
				"System.State":        "Closed",
			},
		})
	})
	mux.HandleFunc("/acme/"+backlogProject+"/_apis/wit/workitemtypes/Issue/states", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{
			"value": []map[string]interface{}{{"name": "Closed", "category": "Completed"}},
		})
	})
	// Addressing the code project is the bug: it must never be reached, and a
	// PR must never be opened for the closed work item.
	mux.HandleFunc("/acme/"+codeProject+"/_apis/wit/workitems/1456", func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("staleness re-check read work item 1456 from the code project %q, want the backlog project %q", codeProject, backlogProject)
		http.NotFound(w, nil)
	})
	mux.HandleFunc("/acme/"+codeProject+"/_apis/git/repositories/web/pullrequests", func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s pull request call — a PR was opened for a closed work item", r.Method)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	previous := newADOProviderForOpenPR
	newADOProviderForOpenPR = func(_ string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(routed.Owner, routed.Project, "token",
			func(p *providers.ADOProvider) { p.BaseURL = server.URL }), nil
	}
	t.Cleanup(func() { newADOProviderForOpenPR = previous })

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "open-pr", root)
	if code != 0 {
		t.Fatalf("open-pr: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no longer open") {
		t.Fatalf("stdout = %q, want the closed backlog work item to abort the PR", stdout)
	}
	result := readOpenPRResult(t, workDir)
	if result["opened"] != "false" {
		t.Fatalf("opened = %q, want \"false\" for a closed work item", result["opened"])
	}
}

// TestOpenPRStalenessNotFoundDiagnosticNamesBacklogProject covers the fail-open
// path's diagnostics: an unresolvable work item still proceeds (a lookup failure
// must never block a legitimate PR), but the warning names the project actually
// addressed so a misrouted backlog is diagnosable from stage logs alone.
func TestOpenPRStalenessNotFoundDiagnosticNamesBacklogProject(t *testing.T) {
	const (
		gaggle         = "example"
		codeProject    = "code-project"
		backlogProject = "backlog-project"
		runID          = "run-3648-missing"
	)
	root := initDemo(t)
	adoBacklogProjectFixture(t, root, gaggle, codeProject, backlogProject)
	recordClaimedIssue(t, root, runID, "1457", "Vanished ADO work")

	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Project: codeProject, Name: "web"}
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv("GOOBERS_GAGGLE", gaggle)
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "implementation")

	mux := http.NewServeMux()
	mux.HandleFunc("/acme/"+backlogProject+"/_apis/wit/workitems/1457", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	opened := 0
	mux.HandleFunc("/acme/"+codeProject+"/_apis/git/repositories/web/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{}})
		case http.MethodPost:
			opened++
			writeJSONResp(t, w, map[string]interface{}{
				"pullRequestId": 9,
				"url":           "api-pr-url",
				"_links":        map[string]interface{}{"web": map[string]string{"href": "https://ado.example/pr/9"}},
			})
		default:
			t.Errorf("unexpected method %s on pullrequests", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	previous := newADOProviderForOpenPR
	newADOProviderForOpenPR = func(_ string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(routed.Owner, routed.Project, "token",
			func(p *providers.ADOProvider) { p.BaseURL = server.URL }), nil
	}
	t.Cleanup(func() { newADOProviderForOpenPR = previous })

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "open-pr", root)
	if code != 0 {
		t.Fatalf("open-pr: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if opened != 1 {
		t.Fatalf("opened %d pull requests, want 1 — an unresolvable work item must fail open", opened)
	}
	if !strings.Contains(stderr, backlogProject) || !strings.Contains(stderr, "1457") {
		t.Fatalf("stderr = %q, want a warning naming work item 1457 and the %s project it was read from", stderr, backlogProject)
	}
}
