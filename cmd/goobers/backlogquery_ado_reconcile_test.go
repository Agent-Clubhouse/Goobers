package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// adoReconcileFake is an Azure DevOps Boards fake that serves an empty
// backlog and records every request, so a test can assert which calls the
// reconcile path made (none) and still let a curation claim scan run.
type adoReconcileFake struct {
	mu       sync.Mutex
	requests []string
}

func (f *adoReconcileFake) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func newADOReconcileProvider(t *testing.T) (*providers.ADOProvider, *adoReconcileFake) {
	t.Helper()
	fake := &adoReconcileFake{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.requests = append(fake.requests, r.Method+" "+r.URL.Path)
		fake.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/_apis/wit/wiql"):
			writeADOJSON(t, w, map[string]any{"workItems": []map[string]int{}})
		case strings.HasSuffix(r.URL.Path, "/_apis/wit/workitemsbatch"):
			writeADOJSON(t, w, map[string]any{"value": []map[string]any{}})
		default:
			http.Error(w, "unexpected ADO request "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	provider := providers.NewADOProvider("org", "backlog", "token", func(p *providers.ADOProvider) {
		p.BaseURL = server.URL
	})
	return provider, fake
}

func adoReconcileEnv(t *testing.T, provider backlogIssueProvider, stdout, stderr *bytes.Buffer) backlogQueryEnv {
	t.Helper()
	root := initDemo(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "backlog", Name: "repo"}
	return backlogQueryEnv{
		root:          root,
		layout:        layoutFor(root),
		repo:          repo,
		backlogRepo:   repo,
		issueProvider: provider,
		stdout:        stdout,
		stderr:        stderr,
	}
}

func readReconciliationResult(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reconciliation result: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode reconciliation result: %v\n%s", err, data)
	}
	return result
}

// assertNotApplicableReconciliation pins the shape a reconcile-backlog stage
// on Azure DevOps must produce for backlog-curation to continue: exit 0, a
// notApplicable marker, zero corrections, and no noWork output — noWork
// would make the executor report ResultNoWork and end the run before
// implementation-feedback, which is exactly the #6104 outcome.
func assertNotApplicableReconciliation(t *testing.T, result map[string]any) {
	t.Helper()
	if result["notApplicable"] != "true" {
		t.Errorf("notApplicable = %v, want \"true\" (result %v)", result["notApplicable"], result)
	}
	if reconciled, ok := result["reconciled"].(float64); !ok || reconciled != 0 {
		t.Errorf("reconciled = %v, want 0 (result %v)", result["reconciled"], result)
	}
	if _, ok := result[executor.OutputNoWork]; ok {
		t.Errorf("result carries %q, which would end backlog-curation before its later stages: %v", executor.OutputNoWork, result)
	}
}

// TestADOBacklogReconcileIsNotApplicable pins Goobers#6104: `backlog-query
// --reconcile` against an Azure DevOps backlog reports not-applicable and
// exits 0 without calling the provider, where it used to fail with the BL-033
// refusal and take every later backlog-curation stage down with it.
func TestADOBacklogReconcileIsNotApplicable(t *testing.T) {
	provider, fake := newADOReconcileProvider(t)
	var stdout, stderr bytes.Buffer
	env := adoReconcileEnv(t, provider, &stdout, &stderr)
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	workDir := t.TempDir()
	t.Chdir(workDir)

	if code := runBacklogQueryMode(backlogQueryModeReconcile, env, nil); code != 0 {
		t.Fatalf("ADO reconcile: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "not applicable on ado") {
		t.Errorf("stdout = %q, want a not-applicable report", stdout.String())
	}
	if strings.Contains(stderr.String(), "BL-033") {
		t.Errorf("stderr = %q, want no BL-033 refusal", stderr.String())
	}
	assertNotApplicableReconciliation(t, readReconciliationResult(t, filepath.Join(workDir, "backlog-reconciliation.json")))
	if paths := fake.paths(); len(paths) != 0 {
		t.Errorf("ADO reconcile made provider calls %v, want none", paths)
	}
}

// TestADOCurationClaimSkipsMetadataReconcile pins the second caller of the
// same pass: backlog-curation's query-backlog stage (`--claim` with
// curation: "true") reconciles metadata inline before scanning. On Azure
// DevOps that inline pass is skipped, so the scan runs; an empty backlog ends
// in the ordinary noWork result rather than the BL-033 failure.
func TestADOCurationClaimSkipsMetadataReconcile(t *testing.T) {
	provider, _ := newADOReconcileProvider(t)
	var stdout, stderr bytes.Buffer
	env := adoReconcileEnv(t, provider, &stdout, &stderr)
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_CURATION", "true")
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "20")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "claimed-items.json")
	t.Setenv(executor.RunIDEnvVar, "ado-curation-run")
	t.Setenv(executor.GaggleEnvVar, "example")
	t.Setenv(executor.WorkflowEnvVar, "backlog-curation")
	workDir := t.TempDir()
	t.Chdir(workDir)

	if code := runBacklogQueryMode(backlogQueryModeClaim, env, nil); code != 0 {
		t.Fatalf("ADO curation claim: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "BL-033") {
		t.Errorf("stderr = %q, want no BL-033 refusal", stderr.String())
	}
	result := readReconciliationResult(t, filepath.Join(workDir, "claimed-items.json"))
	if result[executor.OutputNoWork] != true {
		t.Errorf("claimed-items.json = %v, want the empty-backlog noWork result", result)
	}
}

// TestNonADOBacklogReconcileStillRefuses pins the fix's scope: only an Azure
// DevOps backlog is not-applicable. Any other provider without a GitHub
// issue provider (Gitea) keeps the BL-033 refusal unchanged.
func TestNonADOBacklogReconcileStillRefuses(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := initDemo(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: "org", Name: "repo"}
	env := backlogQueryEnv{
		root: root, layout: layoutFor(root), repo: repo, backlogRepo: repo,
		stdout: &stdout, stderr: &stderr,
	}
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Chdir(t.TempDir())

	if code := runBacklogQueryMode(backlogQueryModeReconcile, env, nil); code == 0 {
		t.Fatalf("Gitea reconcile: code=0 stdout=%q, want the BL-033 refusal", stdout.String())
	}
	if !strings.Contains(stderr.String(), "BL-033") {
		t.Errorf("stderr = %q, want the BL-033 refusal", stderr.String())
	}
}
