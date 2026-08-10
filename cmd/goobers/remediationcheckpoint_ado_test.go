package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// adoCheckpointRecorder captures the label + PR-thread mutations
// remediation-checkpoint's ADO branch performs, so a test can assert the sticky
// remediation-state comment rides on a PR THREAD (post/update) and escalation
// goes through the native PR-LABEL surface — never UpdateWorkItem(PR#).
type adoCheckpointRecorder struct {
	mu                sync.Mutex
	addedLabels       []string
	removedLabels     []string
	threadCreated     bool
	createdThreadBody string
	threadPatched     bool
	patchedPath       string
	patchedContent    string
	workItemTouched   bool
}

// adoCheckpointMux serves the minimal Azure DevOps REST surface the
// remediation-checkpoint ADO branch exercises: the active-PR list (the ONLY
// surface that maps ADO PR labels), the single-PR detail + empty policy set
// (GetPullRequest → PollPullRequest), the PR-thread list/create/update, and the
// PR-label add/remove. A wit/workitems handler fails the test if the
// PR-as-work-item wrong-object write is ever reached on ADO.
func adoCheckpointMux(t *testing.T, repo providers.RepositoryRef, prNumber int, headSHA, baseSHA string, prLabels []string, threadValues []interface{}, rec *adoCheckpointRecorder) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	prBase := "/" + repo.Owner + "/" + repo.Project + "/_apis/git/repositories/" + repo.Name + "/pullrequests"
	id := strconv.Itoa(prNumber)

	labelValues := make([]interface{}, 0, len(prLabels))
	for _, name := range prLabels {
		labelValues = append(labelValues, map[string]interface{}{"name": name})
	}

	// Active-PR list: carries the PR's native labels (GetPullRequest does not).
	mux.HandleFunc(prBase, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{
			map[string]interface{}{
				"pullRequestId":         prNumber,
				"sourceRefName":         "refs/heads/goobers/impl/run-" + id,
				"targetRefName":         "refs/heads/main",
				"createdBy":             map[string]string{"displayName": "goober", "uniqueName": "goober@example.com"},
				"isDraft":               false,
				"lastMergeSourceCommit": map[string]string{"commitId": headSHA},
				"lastMergeTargetCommit": map[string]string{"commitId": baseSHA},
				"reviewers":             []interface{}{},
				"labels":                labelValues,
			},
		}})
	})

	// PR detail (GetPullRequest → PollPullRequest): live head/base/state.
	mux.HandleFunc(prBase+"/"+id, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{
			"pullRequestId":         prNumber,
			"status":                "active",
			"title":                 "Implement PBI 1456",
			"description":           "Implements PBI 1456\n\nFixes #1456",
			"createdBy":             map[string]string{"displayName": "goober", "uniqueName": "goober@example.com"},
			"isDraft":               false,
			"sourceRefName":         "refs/heads/goobers/impl/run-" + id,
			"targetRefName":         "refs/heads/main",
			"lastMergeSourceCommit": map[string]string{"commitId": headSHA},
			"lastMergeTargetCommit": map[string]string{"commitId": baseSHA},
			"reviewers":             []interface{}{},
			"repository": map[string]interface{}{
				"id": "repo-guid", "name": repo.Name,
				"project": map[string]string{"id": "proj-guid", "name": repo.Project},
			},
		})
	})
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{}})
	})

	// PR-thread list (GET) + create (POST) — the sticky remediation-state comment.
	mux.HandleFunc(prBase+"/"+id+"/threads", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSONResp(t, w, map[string]interface{}{"value": threadValues})
		case http.MethodPost:
			var body struct {
				Comments []struct {
					Content string `json:"content"`
				} `json:"comments"`
			}
			decodeFakeJSON(r, &body)
			rec.mu.Lock()
			rec.threadCreated = true
			if len(body.Comments) > 0 {
				rec.createdThreadBody = body.Comments[0].Content
			}
			rec.mu.Unlock()
			writeJSONResp(t, w, map[string]interface{}{
				"id": 11,
				"comments": []interface{}{map[string]interface{}{
					"id": 111, "content": rec.createdThreadBody,
					"author": map[string]string{"displayName": "goober"}, "publishedDate": "2026-08-08T00:00:00Z",
				}},
			})
		default:
			t.Fatalf("threads method = %s, want GET or POST", r.Method)
		}
	})
	// PR-thread comment update (PATCH) — the in-place sticky-comment edit.
	mux.HandleFunc(prBase+"/"+id+"/threads/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Fatalf("thread comment method = %s, want PATCH", r.Method)
		}
		var body struct {
			Content string `json:"content"`
		}
		decodeFakeJSON(r, &body)
		rec.mu.Lock()
		rec.threadPatched = true
		rec.patchedPath = r.URL.Path
		rec.patchedContent = body.Content
		rec.mu.Unlock()
		writeJSONResp(t, w, map[string]interface{}{"id": 100, "content": body.Content})
	})

	// PR-label list (GET, for GetPullRequest's label fetch and the name->id
	// resolution RemovePullRequestLabel does), add (POST), and remove-by-id
	// (DELETE) — the hazard-free escalate/clear. ADO 400s on delete-by-name for
	// colon-bearing names, so removal resolves the id via GET then DELETEs it.
	labelIDs := make([]interface{}, 0, len(prLabels))
	nameByLabelID := map[string]string{}
	for i, name := range prLabels {
		lid := "label-guid-" + strconv.Itoa(i)
		nameByLabelID[lid] = name
		labelIDs = append(labelIDs, map[string]interface{}{"id": lid, "name": name})
	}
	mux.HandleFunc(prBase+"/"+id+"/labels", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSONResp(t, w, map[string]interface{}{"value": labelIDs})
		case http.MethodPost:
			var body struct {
				Name string `json:"name"`
			}
			decodeFakeJSON(r, &body)
			rec.mu.Lock()
			rec.addedLabels = append(rec.addedLabels, body.Name)
			rec.mu.Unlock()
			writeJSONResp(t, w, map[string]interface{}{"id": "label-guid", "name": body.Name})
		default:
			t.Fatalf("labels method = %s, want GET or POST", r.Method)
		}
	})
	mux.HandleFunc(prBase+"/"+id+"/labels/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("label delete method = %s, want DELETE", r.Method)
		}
		// RemovePullRequestLabel deletes by id; map it back to the label name so
		// the assertions still read in terms of names.
		lid := strings.TrimPrefix(r.URL.Path, prBase+"/"+id+"/labels/")
		name := nameByLabelID[lid]
		if name == "" {
			name = lid
		}
		rec.mu.Lock()
		rec.removedLabels = append(rec.removedLabels, name)
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	// The PR-as-work-item wrong-object write must never be reached on ADO.
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/wit/workitems/"+id, func(w http.ResponseWriter, _ *http.Request) {
		rec.mu.Lock()
		rec.workItemTouched = true
		rec.mu.Unlock()
		t.Fatalf("wit/workitems/%s was addressed — the PR-as-work-item wrong-object write ran on ADO", id)
	})
	return mux
}

func stubADOProviderForCheckpointStage(t *testing.T, serverURL string) {
	t.Helper()
	original := newADOProviderForStage
	newADOProviderForStage = func(_ string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(routed.Owner, routed.Project, "token",
			func(p *providers.ADOProvider) { p.BaseURL = serverURL }), nil
	}
	t.Cleanup(func() { newADOProviderForStage = original })
}

func setADOCheckpointStageEnv(t *testing.T, repo providers.RepositoryRef) {
	t.Helper()
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "359")
}

func TestRunRemediationCheckpointADOWaitsForSiblingAndClearsStaleLabel(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	setADOCheckpointStageEnv(t, repo)
	t.Setenv("GOOBERS_INPUT_REMEDIATIONCAUSES", string(remediationCauseSiblingOverlap))

	const headSHA, baseSHA = "head-sha-wait", "base-sha-wait"
	rec := &adoCheckpointRecorder{}
	mux := adoCheckpointMux(t, repo, 359, headSHA, baseSHA, []string{blockedOnSiblingLabel, needsRemediationLabel}, nil, rec)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	stubADOProviderForCheckpointStage(t, server.URL)

	t.Chdir(t.TempDir())
	code, stdout, stderr := runArgs(t, "remediation-checkpoint", "--budget", "2", root)
	if code != 0 {
		t.Fatalf("remediation-checkpoint: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "waiting without consuming remediation budget") {
		t.Fatalf("stdout = %q, want sequencing wait", stdout)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.removedLabels) != 1 || rec.removedLabels[0] != needsRemediationLabel {
		t.Fatalf("removed labels = %v, want [%s]", rec.removedLabels, needsRemediationLabel)
	}
	if rec.threadCreated || rec.threadPatched || rec.workItemTouched {
		t.Fatalf("sequencing wait performed unrelated mutations: %+v", rec)
	}
	if got := readCheckpointResult(t, "checkpoint-result.json")["continueRemediation"]; got != "false" {
		t.Fatalf("continueRemediation = %q, want false", got)
	}
}

// TestRunRemediationCheckpointADOEscalatesUpdatingStickyThread is the headline
// ADO acceptance for §3.5: a forced escalation on a PR that already carries a
// sticky remediation-state comment on its PR THREAD (1) reads that prior state
// back, (2) escalates via the native PR-LABEL surface — adding
// goobers:merge-escalated and clearing goobers:needs-remediation, NEVER
// UpdateWorkItem(PR#) — and (3) records the advanced escalated state (carrying
// the pre-remediation head SHA) by EDITING the same thread comment in place.
func TestRunRemediationCheckpointADOEscalatesUpdatingStickyThread(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	setADOCheckpointStageEnv(t, repo)
	t.Setenv("GOOBERS_INPUT_POLICYEXCLUDED", "true")
	t.Setenv("GOOBERS_INPUT_POLICYEXCLUDEDREASON", `remediation policy "conflict" excludes the only detected cause(s) (substantive)`)

	const headSHA, baseSHA = "head-sha-9f", "base-sha-3c"
	priorSticky, err := remediationStateComment(remediationState{
		Cycles: 2, AttemptsByCause: remediationAttempts{Substantive: 1},
		LastDiffDigest: "sha256:prior", HeadSHA: "older-head", BaseSHA: baseSHA,
	})
	if err != nil {
		t.Fatalf("seed prior sticky state: %v", err)
	}
	threadValues := []interface{}{
		// A system thread (vote/status event) the reader must skip.
		map[string]interface{}{
			"id": 9, "comments": []interface{}{map[string]interface{}{
				"id": 90, "content": "voted", "commentType": "system",
				"author": map[string]string{"displayName": "policy"}, "publishedDate": "2026-08-01T00:00:00Z",
			}},
		},
		// The sticky remediation-state comment a prior cycle posted.
		map[string]interface{}{
			"id": 10, "status": "active", "comments": []interface{}{map[string]interface{}{
				"id": 100, "content": priorSticky, "commentType": "text",
				"author": map[string]string{"displayName": "goober"}, "publishedDate": "2026-08-02T00:00:00Z",
			}},
		},
	}

	rec := &adoCheckpointRecorder{}
	mux := adoCheckpointMux(t, repo, 359, headSHA, baseSHA, []string{needsRemediationLabel}, threadValues, rec)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	stubADOProviderForCheckpointStage(t, server.URL)

	dir := t.TempDir()
	t.Chdir(dir)
	code, stdout, stderr := runArgs(t, "remediation-checkpoint", root)
	if code != 0 {
		t.Fatalf("remediation-checkpoint: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "escalated PR #359") {
		t.Fatalf("stdout = %q, want an escalation for PR #359", stdout)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	// Escalate via PR labels, not a work item.
	if rec.workItemTouched {
		t.Fatal("a wit/workitems write ran on ADO")
	}
	if len(rec.addedLabels) != 1 || rec.addedLabels[0] != remediationEscalatedLabel {
		t.Fatalf("added labels = %v, want [%s]", rec.addedLabels, remediationEscalatedLabel)
	}
	if len(rec.removedLabels) != 1 || rec.removedLabels[0] != needsRemediationLabel {
		t.Fatalf("removed labels = %v, want [%s]", rec.removedLabels, needsRemediationLabel)
	}
	// Sticky state updated IN PLACE on the existing thread comment — not a new
	// thread — addressing the exact composite <pullId>/<threadId>/<commentId>.
	if rec.threadCreated {
		t.Fatalf("a new PR thread was posted; the sticky comment must be edited in place")
	}
	if !rec.threadPatched {
		t.Fatal("the sticky remediation-state thread comment was not updated")
	}
	if !strings.HasSuffix(rec.patchedPath, "/threads/10/comments/100") {
		t.Fatalf("patched path = %q, want the seeded thread comment 10/100", rec.patchedPath)
	}
	// Round-trip: the prior state was read back (cycle 2 → 3) and the new state
	// records THIS cycle's pre-remediation head SHA, the loop's key output.
	state, ok := parseRemediationStateComment(rec.patchedContent)
	if !ok {
		t.Fatalf("patched thread body carries no remediation-state payload: %q", rec.patchedContent)
	}
	if state.Cycles != 3 {
		t.Fatalf("state.Cycles = %d, want 3 (prior 2 read back from the thread + this cycle)", state.Cycles)
	}
	if !state.Escalated || state.HeadSHA != headSHA || state.EscalatedHeadSHA != headSHA {
		t.Fatalf("state = %+v, want escalated recording head %q", state, headSHA)
	}
	if state.EscalationOutcome != remediationOutcomePolicyExcluded {
		t.Fatalf("state.EscalationOutcome = %q, want %q", state.EscalationOutcome, remediationOutcomePolicyExcluded)
	}

	result := readCheckpointResult(t, "checkpoint-result.json")
	if result["continueRemediation"] != "false" {
		t.Fatalf("continueRemediation = %q, want false on escalation", result["continueRemediation"])
	}
	if result["escalationOutcome"] != string(remediationOutcomePolicyExcluded) {
		t.Fatalf("escalationOutcome = %q, want %q", result["escalationOutcome"], remediationOutcomePolicyExcluded)
	}
	if result["headSha"] != headSHA {
		t.Fatalf("headSha = %q, want the pre-remediation head %q", result["headSha"], headSHA)
	}
}

// TestRunRemediationCheckpointADOEscalatesPostingFreshThread proves the other
// half of the sticky-state round-trip: when the PR has no prior remediation-state
// comment on its thread, a reviewer-verdict=fail escalation (--escalate) POSTS a
// NEW PR thread carrying the escalated state (with the head SHA), and still
// escalates through the PR-LABEL surface.
func TestRunRemediationCheckpointADOEscalatesPostingFreshThread(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	setADOCheckpointStageEnv(t, repo)

	const headSHA, baseSHA = "head-sha-fresh", "base-sha-fresh"
	rec := &adoCheckpointRecorder{}
	mux := adoCheckpointMux(t, repo, 359, headSHA, baseSHA, []string{needsRemediationLabel}, []interface{}{}, rec)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	stubADOProviderForCheckpointStage(t, server.URL)

	dir := t.TempDir()
	t.Chdir(dir)
	code, stdout, stderr := runArgs(t, "remediation-checkpoint", "--escalate", "the approach is fundamentally wrong", root)
	if code != 0 {
		t.Fatalf("remediation-checkpoint: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "the approach is fundamentally wrong") {
		t.Fatalf("stdout = %q, want the reviewer-fail reason", stdout)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.workItemTouched {
		t.Fatal("a wit/workitems write ran on ADO")
	}
	if !rec.threadCreated {
		t.Fatal("no PR thread was posted for the escalation on a PR with no prior sticky comment")
	}
	if rec.threadPatched {
		t.Fatal("an in-place update ran; a PR with no prior sticky comment must POST a fresh thread")
	}
	if len(rec.addedLabels) != 1 || rec.addedLabels[0] != remediationEscalatedLabel {
		t.Fatalf("added labels = %v, want [%s]", rec.addedLabels, remediationEscalatedLabel)
	}
	if len(rec.removedLabels) != 1 || rec.removedLabels[0] != needsRemediationLabel {
		t.Fatalf("removed labels = %v, want [%s]", rec.removedLabels, needsRemediationLabel)
	}
	state, ok := parseRemediationStateComment(rec.createdThreadBody)
	if !ok {
		t.Fatalf("posted thread body carries no remediation-state payload: %q", rec.createdThreadBody)
	}
	if !state.Escalated || state.HeadSHA != headSHA {
		t.Fatalf("state = %+v, want escalated recording head %q", state, headSHA)
	}
	if state.Cycles != 1 {
		t.Fatalf("state.Cycles = %d, want 1 (first checkpoint, no prior thread state)", state.Cycles)
	}

	result := readCheckpointResult(t, "checkpoint-result.json")
	if result["continueRemediation"] != "false" {
		t.Fatalf("continueRemediation = %q, want false on escalation", result["continueRemediation"])
	}
	if result["escalationOutcome"] != string(remediationOutcomeDidNotConverge) {
		t.Fatalf("escalationOutcome = %q, want %q for a --escalate reviewer fail", result["escalationOutcome"], remediationOutcomeDidNotConverge)
	}
}
