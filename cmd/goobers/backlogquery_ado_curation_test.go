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
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// TestADOScaffoldBacklogCurationRoutesPastReconcile is the shipped-workflow
// contract for #6104: the backlog-curation workflow `init --template=standard
// --provider=ado` scaffolds starts with a `backlog-query --reconcile` stage
// whose only route is an unconditional next to implementation-feedback, and
// running that stage with the scaffold's own inputs against Azure DevOps
// exits 0 with a success-shaped result (no noWork), so the runner advances.
// Its query-backlog stage, run with the scaffold's own inputs against a
// backlog holding one claimable item, then claims it with real staleness
// evidence and routes on to surface-duplicates.
func TestADOScaffoldBacklogCurationRoutesPastReconcile(t *testing.T) {
	wf := loadADOScaffoldCuration(t)
	reconcile := adoScaffoldCurationStart(t, wf)

	provider, _ := newADOReconcileProvider(t)
	var out, errOut bytes.Buffer
	env := adoReconcileEnv(t, provider, &out, &errOut)
	setStageInputs(t, reconcile.Inputs)
	workDir := t.TempDir()
	t.Chdir(workDir)
	if code := runBacklogQueryMode(backlogQueryModeReconcile, env, nil); code != 0 {
		t.Fatalf("scaffold reconcile-backlog on ADO: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	assertNotApplicableReconciliation(t, readReconciliationResult(t, filepath.Join(workDir, reconcile.Inputs["resultFile"])))

	query := curationTask(t, wf, "query-backlog")
	if query.Run == nil || strings.Join(query.Run.Command, " ") != "goobers backlog-query --claim" ||
		query.Next != "surface-duplicates" || query.Inputs["curation"] != "true" {
		t.Fatalf("scaffold query-backlog = %+v, want a curation claim routing to surface-duplicates", query)
	}
	for key := range reconcile.Inputs {
		t.Setenv(executor.InputEnvVar(key), "")
	}
	setStageInputs(t, query.Inputs)
	claimed := runADOCurationClaim(t, filepath.Join(workDir, "query"), query.Inputs["resultFile"])
	assertADOClaimStaleness(t, claimed, query.Inputs["staleAfterDays"])
}

// TestADOCurationClaimComputesStaleness is the regression test for the
// query-backlog crash #6104 surfaced: a curation claim (`--claim` with
// curation: "true") that claims at least one Azure DevOps work item used to
// dereference a nil GitHub provider while computing staleness. It now exits
// 0 with the claimed item carrying a real staleness signal computed from the
// work item's comments, with Goobers' own comments (matched by identity ID)
// excluded from meaningful activity.
func TestADOCurationClaimComputesStaleness(t *testing.T) {
	setStageInputs(t, map[string]string{
		"trustLabel":     "goobers:approved",
		"curation":       "true",
		"maxItems":       "20",
		"resultFile":     "claimed-items.json",
		"staleAfterDays": "90",
	})
	assertADOClaimStaleness(t, runADOCurationClaim(t, t.TempDir(), "claimed-items.json"), "90")
}

// TestStalenessEnrichmentMarksUnavailableProvider pins the no-panic guard: a
// backlog with no comment surface gets no staleness signal and the explicit
// "provider" marker, never a zero-valued signal that reads as fresh.
func TestStalenessEnrichmentMarksUnavailableProvider(t *testing.T) {
	items := []providers.WorkItem{{ID: "9"}}
	got, err := enrichClaimedItemsWithStaleness(
		t.Context(), backlogStalenessProvider(backlogQueryEnv{}), providers.RepositoryRef{},
		items, time.Now(), backlogStalenessPolicy{thresholdDays: 90},
	)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(got) != 1 || got[0].Staleness != nil || got[0].StalenessUnavailable != stalenessUnavailableProvider {
		t.Fatalf("enriched = %+v, want no signal and the provider-unavailable marker", got)
	}
	data, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"staleness"`) || !strings.Contains(string(data), `"stalenessUnavailable":"provider"`) {
		t.Errorf("marshaled = %s, want stalenessUnavailable and no staleness object", data)
	}
}

func setStageInputs(t *testing.T, inputs map[string]string) {
	t.Helper()
	for key, value := range inputs {
		t.Setenv(executor.InputEnvVar(key), value)
	}
}

const adoCurationSelfID = "00000000-0000-0000-0000-0000000005e1"

// adoCurationHumanCommentAt is the only meaningful activity on the claimable
// fake item: 200 days old, so the item is stale at a 90-day threshold unless
// Goobers' own fresh claim comment is miscounted as activity.
var adoCurationHumanCommentAt = time.Now().UTC().Add(-200 * 24 * time.Hour).Truncate(time.Second)

// adoClaimableBacklog is one approved, claimable ADO work item (42) whose
// comment history holds one old human comment. Comments the claim posts are
// attributed to the authenticated identity and stamped now.
type adoClaimableBacklog struct {
	t        *testing.T
	mu       sync.Mutex
	tags     string
	revision int
	created  string
	comments []map[string]any
}

func (b *adoClaimableBacklog) item() map[string]any {
	return map[string]any{"id": 42, "rev": b.revision, "fields": map[string]any{
		"System.WorkItemType": "Issue", "System.Title": "ADO curation item", "System.State": "Active",
		"System.Tags": b.tags, "System.CreatedDate": b.created, "System.ChangedDate": b.created,
	}}
}

func (b *adoClaimableBacklog) serveComments(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Method != http.MethodPost {
		writeADOJSON(b.t, w, map[string]any{"comments": b.comments})
		return
	}
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		b.t.Errorf("decode comment: %v", err)
	}
	b.comments = append(b.comments, map[string]any{
		"id": len(b.comments) + 1, "text": body["text"], "createdDate": time.Now().UTC().Format(time.RFC3339),
		"createdBy": map[string]string{"id": adoCurationSelfID, "displayName": "Goobers Bot"},
	})
	writeADOJSON(b.t, w, b.comments[len(b.comments)-1])
}

func (b *adoClaimableBacklog) serveWorkItem(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Method == http.MethodPatch {
		var patch []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			b.t.Errorf("decode work item patch: %v", err)
		}
		for _, operation := range patch {
			if operation["path"] == "/fields/System.Tags" {
				b.tags, _ = operation["value"].(string)
			}
		}
		b.revision++
	}
	writeADOJSON(b.t, w, b.item())
}

func newADOClaimableBacklog(t *testing.T) *providers.ADOProvider {
	t.Helper()
	b := &adoClaimableBacklog{
		t: t, tags: "goobers:approved", revision: 1,
		created: adoCurationHumanCommentAt.Add(-24 * time.Hour).Format(time.RFC3339),
		comments: []map[string]any{{
			"id": 1, "text": "still wanted", "createdDate": adoCurationHumanCommentAt.Format(time.RFC3339),
			"createdBy": map[string]string{"id": "11111111-1111-1111-1111-111111111111", "displayName": "A Human"},
		}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/backlog/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		writeADOJSON(t, w, map[string]any{"workItems": []map[string]int{{"id": 42}}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workitemtypes/Issue/states", func(w http.ResponseWriter, r *http.Request) {
		writeADOJSON(t, w, map[string]any{"value": []map[string]string{{"name": "Active", "category": "InProgress"}}})
	})
	mux.HandleFunc("/org/_apis/connectionData", func(w http.ResponseWriter, r *http.Request) {
		writeADOJSON(t, w, map[string]any{"authenticatedUser": map[string]any{
			"id": adoCurationSelfID, "providerDisplayName": "Goobers Bot",
		}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workitemsbatch", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		writeADOJSON(t, w, map[string]any{"value": []map[string]any{b.item()}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workItems/42/comments", b.serveComments)
	mux.HandleFunc("/org/backlog/_apis/wit/workitems/42", b.serveWorkItem)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return providers.NewADOProvider("org", "backlog", "token", func(p *providers.ADOProvider) {
		p.BaseURL = server.URL
	})
}

// runADOCurationClaim runs a curation `--claim` against newADOClaimableBacklog
// from workDir with the stage inputs already set, and returns the decoded
// claimed items.
func runADOCurationClaim(t *testing.T, workDir, resultFile string) []curationClaimedItem {
	t.Helper()
	t.Setenv(executor.RunIDEnvVar, "ado-curation-run")
	t.Setenv(executor.GaggleEnvVar, "example")
	t.Setenv(executor.WorkflowEnvVar, "backlog-curation")
	var stdout, stderr bytes.Buffer
	env := adoReconcileEnv(t, newADOClaimableBacklog(t), &stdout, &stderr)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workDir)
	if code := runBacklogQueryMode(backlogQueryModeClaim, env, nil); code != 0 {
		t.Fatalf("ADO curation claim: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(workDir, resultFile))
	if err != nil {
		t.Fatalf("read %s: %v", resultFile, err)
	}
	var claimed []curationClaimedItem
	if err := json.Unmarshal(data, &claimed); err != nil {
		t.Fatalf("decode %s: %v\n%s", resultFile, err, data)
	}
	return claimed
}

func assertADOClaimStaleness(t *testing.T, claimed []curationClaimedItem, thresholdDays string) {
	t.Helper()
	if len(claimed) != 1 || claimed[0].ID != "42" {
		t.Fatalf("claimed = %+v, want work item 42", claimed)
	}
	got := claimed[0]
	if got.Staleness == nil || got.StalenessUnavailable != "" {
		t.Fatalf("item 42 staleness = %v unavailable = %q, want a computed signal", got.Staleness, got.StalenessUnavailable)
	}
	if !got.Staleness.LastMeaningfulActivityAt.Equal(adoCurationHumanCommentAt) {
		t.Errorf("last meaningful activity = %s, want the human comment at %s (Goobers' own claim comment must not count)",
			got.Staleness.LastMeaningfulActivityAt, adoCurationHumanCommentAt)
	}
	if !got.Staleness.Stale || thresholdDays != "90" || got.Staleness.ThresholdDays != 90 {
		t.Errorf("staleness = %+v (declared threshold %q), want stale at a 90-day threshold", *got.Staleness, thresholdDays)
	}
}

func loadADOScaffoldCuration(t *testing.T) apiv1.Workflow {
	t.Helper()
	scaffold := filepath.Join(t.TempDir(), "ado")
	code, stdout, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--pr-ci", scaffold)
	if code != 0 {
		t.Fatalf("ADO init code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	set, report, err := instance.LoadConfigDir(filepath.Join(scaffold, "config"))
	if err != nil || report.HasErrors() {
		t.Fatalf("load scaffold: %v report=%+v", err, report)
	}
	for _, wf := range set.Workflows {
		if wf.Name == "backlog-curation" {
			return wf
		}
	}
	t.Fatal("ADO scaffold has no backlog-curation workflow")
	return apiv1.Workflow{}
}

func curationTask(t *testing.T, wf apiv1.Workflow, name string) apiv1.Task {
	t.Helper()
	for _, task := range wf.Spec.Tasks {
		if task.Name == name {
			return task
		}
	}
	t.Fatalf("backlog-curation has no task %q", name)
	return apiv1.Task{}
}

// adoScaffoldCurationStart returns the scaffolded backlog-curation start
// task after asserting it is the reconcile stage and routes unconditionally
// (no gate) to implementation-feedback.
func adoScaffoldCurationStart(t *testing.T, wf apiv1.Workflow) apiv1.Task {
	t.Helper()
	task := curationTask(t, wf, wf.Spec.Start)
	if task.Run == nil || strings.Join(task.Run.Command, " ") != "goobers backlog-query --reconcile" {
		t.Fatalf("backlog-curation start %q runs %+v, want goobers backlog-query --reconcile", task.Name, task.Run)
	}
	if task.Next != "implementation-feedback" || task.ContinueOnError {
		t.Fatalf("backlog-curation start %q next=%q continueOnError=%t, want an unconditional next to implementation-feedback", task.Name, task.Next, task.ContinueOnError)
	}
	for _, gate := range wf.Spec.Gates {
		if gate.Name == task.Next {
			t.Fatalf("backlog-curation start routes through gate %q, want a direct task handoff", gate.Name)
		}
	}
	if task.Inputs["resultFile"] == "" || task.Inputs["trustLabel"] == "" {
		t.Fatalf("backlog-curation start inputs = %v, want resultFile and trustLabel", task.Inputs)
	}
	return task
}
