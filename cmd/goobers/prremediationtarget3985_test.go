package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// Issue #3985: `goobers run --pr <n> pr-remediation` is delivered as a
// synthetic pull_request webhook, so the operator's argument reaches the lane
// only as the run's trigger reference. Before this change the remediation
// selectors ignored that reference entirely and ranked by their own
// remediation priority, so a targeted run would start and then remediate
// whichever PR policy preferred. These tests pin the three properties the fix
// has to hold: a targeted run selects EXACTLY the trigger's PR, an
// unselectable target refuses with a named reason instead of substituting
// another PR, and an untargeted (scheduled) run is byte-for-byte unchanged.

func remediationTriggerRefFor(number int) string {
	return webhookhttp.TriggerRef(webhookhttp.Delivery{Event: "pull_request", PullNumber: number})
}

func TestRemediationTargetFromTriggerRef(t *testing.T) {
	tests := []struct {
		name     string
		ref      string
		targeted bool
		number   int
	}{
		{name: "pull request delivery", ref: remediationTriggerRefFor(3968), targeted: true, number: 3968},
		{name: "schedule tick", ref: "pr-remediation"},
		{name: "untargeted pull_request signal", ref: webhookhttp.SignalName("pull_request")},
		{name: "unrelated webhook event", ref: webhookhttp.SignalName("push")},
		{name: "empty", ref: ""},
		{name: "malformed number", ref: webhookhttp.SignalName("pull_request") + "#not-a-number"},
		{name: "non-positive number", ref: webhookhttp.SignalName("pull_request") + "#0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := remediationTargetFromTriggerRef(tt.ref)
			if got.targeted != tt.targeted || got.number != tt.number {
				t.Fatalf("remediationTargetFromTriggerRef(%q) = %+v, want targeted=%t number=%d",
					tt.ref, got, tt.targeted, tt.number)
			}
		})
	}
}

func TestRemediationTargetApplyRestrictsSelectionOrRefuses(t *testing.T) {
	listed := []providers.PullRequestSummary{{Number: 10}, {Number: 11}, {Number: 12}}
	filtered := []providers.PullRequestSummary{{Number: 10}, {Number: 11}}
	unclaimed := []providers.PullRequestSummary{{Number: 10}}
	candidates := []providers.PullRequestSummary{{Number: 10}}
	stages := func(final []providers.PullRequestSummary) []remediationTargetStage {
		return []remediationTargetStage{
			{prs: listed, reason: remediationTargetUnlistedReason("main", "goobers/")},
			{prs: filtered, reason: remediationTargetFilteredReason},
			{prs: unclaimed, reason: remediationTargetClaimedReason},
			{prs: final, reason: remediationTargetIneligibleReason},
		}
	}

	t.Run("untargeted selection is unchanged", func(t *testing.T) {
		got, refusal := remediationTarget{}.apply(stages(candidates)...)
		if refusal != "" {
			t.Fatalf("refusal = %q, want none for an untargeted run", refusal)
		}
		if len(got) != len(candidates) || got[0].Number != 10 {
			t.Fatalf("candidates = %+v, want the ranked set untouched", got)
		}
	})

	t.Run("targeted selection keeps exactly the target", func(t *testing.T) {
		ranked := []providers.PullRequestSummary{{Number: 10}, {Number: 12}}
		got, refusal := remediationTarget{number: 12, targeted: true}.apply(
			remediationTargetStage{prs: listed, reason: remediationTargetUnlistedReason("main", "goobers/")},
			remediationTargetStage{prs: listed, reason: remediationTargetFilteredReason},
			remediationTargetStage{prs: listed, reason: remediationTargetClaimedReason},
			remediationTargetStage{prs: ranked, reason: remediationTargetIneligibleReason},
		)
		if refusal != "" {
			t.Fatalf("refusal = %q, want none when the target is selectable", refusal)
		}
		if len(got) != 1 || got[0].Number != 12 {
			t.Fatalf("candidates = %+v, want exactly the targeted PR #12 (not policy's #10)", got)
		}
	})

	refusals := []struct {
		name   string
		number int
		want   string
	}{
		{name: "never listed", number: 99, want: "selection scope"},
		{name: "filtered out", number: 12, want: remediationTargetFilteredReason},
		{name: "claimed elsewhere", number: 11, want: remediationTargetClaimedReason},
	}
	for _, tt := range refusals {
		t.Run("refuses target "+tt.name, func(t *testing.T) {
			got, refusal := remediationTarget{number: tt.number, targeted: true}.apply(stages(candidates)...)
			if len(got) != 0 {
				t.Fatalf("candidates = %+v, want no fallback selection", got)
			}
			if !strings.Contains(refusal, tt.want) ||
				!strings.Contains(refusal, "#"+strconv.Itoa(tt.number)) {
				t.Fatalf("refusal = %q, want it to name PR #%d and %q", refusal, tt.number, tt.want)
			}
		})
	}

	t.Run("refuses ineligible target", func(t *testing.T) {
		got, refusal := remediationTarget{number: 10, targeted: true}.apply(
			remediationTargetStage{prs: listed, reason: remediationTargetUnlistedReason("main", "goobers/")},
			remediationTargetStage{prs: nil, reason: remediationTargetIneligibleReason},
		)
		if len(got) != 0 || !strings.Contains(refusal, remediationTargetIneligibleReason) {
			t.Fatalf("candidates = %+v, refusal = %q, want an ineligible-target refusal", got, refusal)
		}
	})

	t.Run("fails closed with no stages", func(t *testing.T) {
		got, refusal := remediationTarget{number: 10, targeted: true}.apply()
		if len(got) != 0 || refusal == "" {
			t.Fatalf("candidates = %+v, refusal = %q, want a refusal rather than an unconstrained selection", got, refusal)
		}
	})
}

// TestUpdateBehindPRHonorsDispatchTarget is the headline #3985 acceptance for
// the lane's entry stage. Two PRs carry goobers:needs-remediation; policy ranks
// the lower-numbered #54 first. The scheduled control proves that ranking is
// untouched, and the targeted run proves `--pr 55` selects #55 — the argument
// the operator actually passed — and acts on it.
func TestUpdateBehindPRHonorsDispatchTarget(t *testing.T) {
	t.Run("scheduled tick still selects by remediation priority", func(t *testing.T) {
		mergeable := true
		state := &updateBehindServer{
			mergeable:               &mergeable,
			labels:                  []string{needsRemediationLabel},
			includeEarlierCandidate: true,
		}
		root, workspace := setupUpdateBehindPRTest(t, state)
		code, stdout, stderr, result := invokeUpdateBehindPRTest(t, root, workspace)
		if code != 0 {
			t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if result["selectedNumber"] != "54" {
			t.Fatalf("selectedNumber = %q, want policy's pick 54 on an untargeted tick", result["selectedNumber"])
		}
		if state.updateCalls != 0 {
			t.Fatalf("update-branch calls on PR 55 = %d, want 0 — policy selected 54", state.updateCalls)
		}
	})

	t.Run("targeted run selects the dispatched pull request", func(t *testing.T) {
		mergeable := true
		state := &updateBehindServer{
			mergeable:               &mergeable,
			labels:                  []string{needsRemediationLabel},
			includeEarlierCandidate: true,
		}
		root, workspace := setupUpdateBehindPRTest(t, state)
		t.Setenv(executor.TriggerRefEnvVar, remediationTriggerRefFor(55))

		code, stdout, stderr, result := invokeUpdateBehindPRTest(t, root, workspace)
		if code != 0 {
			t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if result["selectedNumber"] != "55" {
			t.Fatalf("selectedNumber = %q, want the trigger's PR 55, not policy's 54", result["selectedNumber"])
		}
		if state.updateCalls != 1 {
			t.Fatalf("update-branch calls on PR 55 = %d, want 1 — the targeted PR was acted on", state.updateCalls)
		}
		if hasAnyLabel(state.labels, []string{needsRemediationLabel}) {
			t.Fatalf("labels = %v, want %s cleared on the targeted PR", state.labels, needsRemediationLabel)
		}
	})
}

// TestUpdateBehindPRRefusesUnselectableTarget covers the fail-closed half:
// a targeted PR that is ineligible, or absent from the lane's scope entirely,
// ends the run as an explicit no-work naming the reason. It must never fall
// back to the PR policy would otherwise have picked.
func TestUpdateBehindPRRefusesUnselectableTarget(t *testing.T) {
	tests := []struct {
		name   string
		target int
		want   string
	}{
		{
			name:   "ineligible target",
			target: 56,
			want:   "does not need remediation this cycle",
		},
		{
			name:   "target outside the lane's scope",
			target: 9999,
			want:   "is not an open pull request in this lane's selection scope",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mergeable := true
			state := &updateBehindServer{
				mergeable:       &mergeable,
				labels:          []string{needsRemediationLabel},
				unselectedCount: 2,
			}
			root, workspace := setupUpdateBehindPRTest(t, state)
			t.Setenv(executor.TriggerRefEnvVar, remediationTriggerRefFor(tt.target))

			code, stdout, stderr := runArgs(t, "update-behind-pr", root)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q, want a clean no-work refusal", code, stdout, stderr)
			}
			if !strings.Contains(stdout, "no work: targeted PR #"+strconv.Itoa(tt.target)) ||
				!strings.Contains(stdout, tt.want) {
				t.Fatalf("stdout = %q, want a no-work naming PR #%d and %q", stdout, tt.target, tt.want)
			}
			if state.updateCalls != 0 {
				t.Fatalf("update-branch calls = %d, want 0 — the eligible PR 55 must not be substituted", state.updateCalls)
			}
			if !hasAnyLabel(state.labels, []string{needsRemediationLabel}) {
				t.Fatalf("labels = %v, want PR 55 untouched by a refused targeted run", state.labels)
			}
			data, err := os.ReadFile(filepath.Join(workspace, "update-behind-result.json"))
			if err != nil {
				t.Fatalf("read result: %v", err)
			}
			var result map[string]interface{}
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
			if result[executor.OutputNoWork] != true {
				t.Fatalf("result = %v, want the declared no-work outcome", result)
			}
			if _, selected := result["selectedNumber"]; selected {
				t.Fatalf("result = %v, want no PR selected by a refused targeted run", result)
			}
		})
	}
}

// targetedRemediationPR is one fixture pull request in the two-candidate
// gather-pr-context fixture below.
type targetedRemediationPR struct {
	number  int
	head    string
	headSHA string
	labels  []string
}

// targetedRemediationServer is a minimal GitHub fake serving several open PRs
// that all clear gather-pr-context's remediation eligibility, which is what
// makes "policy would pick a different PR" observable.
type targetedRemediationServer struct {
	owner, repo string
	base        string
	baseSHA     string
	prs         []targetedRemediationPR
}

func (s *targetedRemediationServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	prefix := "/repos/" + s.owner + "/" + s.repo
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		writeFakeJSON(w, map[string]string{"login": "merge-review-bot"})
	})
	mux.HandleFunc(prefix+"/git/ref/", func(w http.ResponseWriter, _ *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"object": map[string]string{"sha": s.baseSHA}})
	})
	mux.HandleFunc(prefix+"/pulls", func(w http.ResponseWriter, _ *http.Request) {
		payload := make([]map[string]interface{}, 0, len(s.prs))
		for _, pr := range s.prs {
			payload = append(payload, map[string]interface{}{
				"number": pr.number, "state": "open", "draft": false,
				"html_url": fmt.Sprintf("https://github.test/%s/%s/pull/%d", s.owner, s.repo, pr.number),
				"head":     map[string]interface{}{"ref": pr.head, "sha": pr.headSHA},
				"base":     map[string]interface{}{"ref": s.base, "sha": s.baseSHA},
				"labels":   labelsJSON(pr.labels),
			})
		}
		writeFakeJSON(w, payload)
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		calls := 0
		serveBulkCheckStates(t, w, r, &calls, map[string]string{})
	})
	for _, pr := range s.prs {
		number := pr.number
		mux.HandleFunc(fmt.Sprintf("%s/issues/%d/comments", prefix, number), func(w http.ResponseWriter, _ *http.Request) {
			writeFakeJSON(w, []map[string]interface{}{})
		})
		mux.HandleFunc(fmt.Sprintf("%s/issues/%d", prefix, number), func(w http.ResponseWriter, _ *http.Request) {
			writeFakeJSON(w, map[string]interface{}{
				"number": number, "state": "open", "title": "fixture PR",
				"html_url": fmt.Sprintf("https://github.test/%s/%s/pull/%d", s.owner, s.repo, number),
				"labels":   []interface{}{},
			})
		})
		mux.HandleFunc(fmt.Sprintf("%s/commits/%s/status", prefix, pr.headSHA), func(w http.ResponseWriter, _ *http.Request) {
			writeFakeJSON(w, map[string]interface{}{"state": "success", "statuses": []interface{}{}})
		})
		mux.HandleFunc(fmt.Sprintf("%s/commits/%s/check-runs", prefix, pr.headSHA), func(w http.ResponseWriter, _ *http.Request) {
			writeFakeJSON(w, map[string]interface{}{"check_runs": []interface{}{}})
		})
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// initTargetedRemediationOrigin seeds a bare origin with main plus one branch
// per name, each carrying a distinct commit, and then advances main past every
// branch point so each PR is genuinely behind its base.
func initTargetedRemediationOrigin(t *testing.T, branches ...string) (origin string, heads map[string]string, baseSHA string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	runGitT(t, root, "init", "--bare", "-b", "main", origin)

	work := filepath.Join(root, "work")
	runGitT(t, root, "clone", origin, work)
	runGitT(t, work, "config", "user.name", "seed")
	runGitT(t, work, "config", "user.email", "seed@example.com")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGitT(t, work, "add", "README.md")
	runGitT(t, work, "commit", "-m", "seed")
	runGitT(t, work, "push", "origin", "main")

	heads = make(map[string]string, len(branches))
	for i, branch := range branches {
		runGitT(t, work, "checkout", "main")
		runGitT(t, work, "checkout", "-b", branch)
		file := fmt.Sprintf("feature-%d.txt", i)
		if err := os.WriteFile(filepath.Join(work, file), []byte("pr work\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
		runGitT(t, work, "add", file)
		runGitT(t, work, "commit", "-m", "pr work "+branch)
		runGitT(t, work, "push", "origin", branch)
		heads[branch] = strings.TrimSpace(runGitOutputT(t, work, "rev-parse", "HEAD"))
	}

	runGitT(t, work, "checkout", "main")
	if err := os.WriteFile(filepath.Join(work, "unrelated.txt"), []byte("main moved on\n"), 0o644); err != nil {
		t.Fatalf("write unrelated file: %v", err)
	}
	runGitT(t, work, "add", "unrelated.txt")
	runGitT(t, work, "commit", "-m", "main moved on")
	runGitT(t, work, "push", "origin", "main")
	baseSHA = strings.TrimSpace(runGitOutputT(t, work, "rev-parse", "HEAD"))
	return origin, heads, baseSHA
}

// setupTargetedGatherPRContext wires the two-candidate fixture into a real
// worktree and returns the instance root and the worktree path the stage runs
// in. Both candidate branches exist in the origin, so either selection is a
// legal checkout — the test's assertion is about WHICH one the stage picks.
func setupTargetedGatherPRContext(t *testing.T, runID string) (root, workDir string) {
	t.Helper()
	const (
		lowerBranch = "goobers/impl/lower"
		upperBranch = "goobers/impl/upper"
	)
	origin, heads, baseSHA := initTargetedRemediationOrigin(t, lowerBranch, upperBranch)

	srv := &targetedRemediationServer{
		owner: "your-org", repo: "your-repo", base: "main", baseSHA: baseSHA,
		prs: []targetedRemediationPR{
			{number: 70, head: lowerBranch, headSHA: heads[lowerBranch], labels: []string{needsRemediationLabel}},
			{number: 71, head: upperBranch, headSHA: heads[upperBranch], labels: []string{needsRemediationLabel}},
		},
	}
	server := srv.start(t)
	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: server.URL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: runID, BaseRef: "main",
		Branch: "goobers/pr-remediation/" + runID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	root = initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Chdir(wt.Path)
	return root, wt.Path
}

func remediationBriefSelectedNumber(t *testing.T, workDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workDir, remediationBriefResultFile))
	if err != nil {
		t.Fatalf("read %s: %v", remediationBriefResultFile, err)
	}
	var brief apiv1.RemediationBrief
	if err := json.Unmarshal(data, &brief); err != nil {
		t.Fatalf("unmarshal %s: %v", remediationBriefResultFile, err)
	}
	return brief.SelectedNumber
}

// remediationBriefResult reads the stage's result artifact as a generic map so
// refusal tests can assert on the no-work/error shape the runner consumes.
func remediationBriefResult(t *testing.T, workDir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workDir, remediationBriefResultFile))
	if err != nil {
		t.Fatalf("read %s: %v", remediationBriefResultFile, err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal %s: %v", remediationBriefResultFile, err)
	}
	if _, ok := result["selectedNumber"]; ok {
		t.Fatalf("%s selected a pull request (%s), want none for a refused run", remediationBriefResultFile, data)
	}
	return result
}

// TestGatherPRContextHonorsDispatchTarget is #3985's acceptance for the
// remediation lane's second selector: the stage that self-selects when no
// update-behind-pr handoff pinned a candidate. Two PRs are equally eligible;
// policy ranks #70 first. The scheduled control proves that ranking still
// holds, and the targeted run proves `--pr 71` selects and checks out #71.
func TestGatherPRContextHonorsDispatchTarget(t *testing.T) {
	t.Run("scheduled tick still selects by remediation priority", func(t *testing.T) {
		root, workDir := setupTargetedGatherPRContext(t, "run-3985-schedule")
		code, stdout, stderr := runArgs(t, "gather-pr-context", root)
		if code != 0 {
			t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if got := remediationBriefSelectedNumber(t, workDir); got != "70" {
			t.Fatalf("selectedNumber = %q, want policy's pick 70 on an untargeted tick", got)
		}
	})

	t.Run("targeted run selects the dispatched pull request", func(t *testing.T) {
		root, workDir := setupTargetedGatherPRContext(t, "run-3985-targeted")
		t.Setenv(executor.TriggerRefEnvVar, remediationTriggerRefFor(71))

		code, stdout, stderr := runArgs(t, "gather-pr-context", root)
		if code != 0 {
			t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if got := remediationBriefSelectedNumber(t, workDir); got != "71" {
			t.Fatalf("selectedNumber = %q, want the trigger's PR 71, not policy's 70", got)
		}
		if branch := strings.TrimSpace(runGitOutputT(t, workDir, "symbolic-ref", "--short", "HEAD")); branch != "goobers/impl/upper" {
			t.Fatalf("checked-out branch = %q, want the targeted PR's own branch", branch)
		}
	})
}

// TestGatherPRContextRefusesUnselectableTarget proves the fail-closed half on
// the remediation lane's context stage: a target outside the lane's scope ends
// the run as an explicit no-work naming the reason, selecting no pull request
// and checking out no substitute branch.
func TestGatherPRContextRefusesUnselectableTarget(t *testing.T) {
	root, workDir := setupTargetedGatherPRContext(t, "run-3985-refused")
	t.Setenv(executor.TriggerRefEnvVar, remediationTriggerRefFor(9999))

	code, stdout, stderr := runArgs(t, "gather-pr-context", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q, want a clean no-work refusal", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work: targeted PR #9999") ||
		!strings.Contains(stdout, "is not an open pull request in this lane's selection scope") {
		t.Fatalf("stdout = %q, want a no-work naming the unselectable target", stdout)
	}
	if result := remediationBriefResult(t, workDir); result["noWork"] != true {
		t.Fatalf("result = %v, want an explicit no-work result for a refused target", result)
	}
	if branch := strings.TrimSpace(runGitOutputT(t, workDir, "symbolic-ref", "--short", "HEAD")); branch != "goobers/pr-remediation/run-3985-refused" {
		t.Fatalf("checked-out branch = %q, want the run's own branch — no substitute PR was checked out", branch)
	}
}

// TestGatherPRContextRefusesTargetPinnedToAnotherPullRequest closes the
// handoff seam. update-behind-pr restricts its own selection to the trigger's
// PR, so a selectedNumber input naming a different PR means upstream state
// disagrees with the operator's argument: refuse the run rather than remediate
// a pull request nobody targeted.
func TestGatherPRContextRefusesTargetPinnedToAnotherPullRequest(t *testing.T) {
	root, workDir := setupTargetedGatherPRContext(t, "run-3985-mismatch")
	t.Setenv(executor.TriggerRefEnvVar, remediationTriggerRefFor(71))
	t.Setenv(executor.InputEnvVar("selectedNumber"), "70")

	code, stdout, stderr := runArgs(t, "gather-pr-context", root)
	if code != 1 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q, want 1 (fail closed)", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "targets PR #71 but is pinned to PR #70") {
		t.Fatalf("stderr = %q, want the mismatch named", stderr)
	}
	if result := remediationBriefResult(t, workDir); result["errorMessage"] == nil ||
		!strings.Contains(fmt.Sprint(result["errorMessage"]), "targets PR #71 but is pinned to PR #70") {
		t.Fatalf("result = %v, want a failed result naming the mismatch", result)
	}
}

// setupTargetedGatherPRContextADO is the ADO analog of the two-candidate GitHub
// fixture: two active pull requests, both carrying the needs-remediation label
// read straight off ADO's ListPullRequests, so the lower-numbered one wins the
// ranking and any targeted selection is provably not the policy pick.
func setupTargetedGatherPRContextADO(t *testing.T, runID string) (root, workDir string) {
	t.Helper()
	const (
		lowerBranch = "goobers/impl/ado-lower"
		upperBranch = "goobers/impl/ado-upper"
	)
	origin, heads, baseSHA := initTargetedRemediationOrigin(t, lowerBranch, upperBranch)

	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	prBase := "/" + repo.Owner + "/" + repo.Project + "/_apis/git/repositories/" + repo.Name + "/pullrequests"
	adoPR := func(id int, branch string) map[string]interface{} {
		return map[string]interface{}{
			"pullRequestId":         id,
			"status":                "active",
			"sourceRefName":         "refs/heads/" + branch,
			"targetRefName":         "refs/heads/main",
			"isDraft":               false,
			"createdBy":             map[string]string{"displayName": "goober", "uniqueName": "goober@example.com"},
			"labels":                []map[string]string{{"name": needsRemediationLabel}},
			"lastMergeSourceCommit": map[string]string{"commitId": heads[branch]},
			"lastMergeTargetCommit": map[string]string{"commitId": baseSHA},
			"_links":                map[string]interface{}{"web": map[string]string{"href": fmt.Sprintf("https://dev.azure.test/pr/%d", id)}},
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc(prBase, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{
			adoPR(359, lowerBranch), adoPR(360, upperBranch),
		}})
	})
	for _, id := range []int{359, 360} {
		mux.HandleFunc(prBase+"/"+strconv.Itoa(id)+"/threads", func(w http.ResponseWriter, _ *http.Request) {
			writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{}})
		})
	}
	mux.HandleFunc("/"+repo.Owner+"/_apis/connectionData", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{
			"authenticatedUser": map[string]string{"providerDisplayName": "merge-review-bot"},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	original := newADOProviderForStage
	newADOProviderForStage = func(_ string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(routed.Owner, routed.Project, "token",
			func(p *providers.ADOProvider) { p.BaseURL = server.URL }), nil
	}
	t.Cleanup(func() { newADOProviderForStage = original })

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: runID, BaseRef: "main",
		Branch: "goobers/pr-remediation/" + runID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Chdir(wt.Path)
	return root, wt.Path
}

// TestGatherPRContextADOHonorsDispatchTarget proves the fix covers the lane's
// third selector — gather-pr-context's ADO branch — with the same guarantees as
// the GitHub branch: unchanged scheduled ranking, exact targeted selection, and
// a fail-closed refusal for a target outside the lane's scope.
func TestGatherPRContextADOHonorsDispatchTarget(t *testing.T) {
	t.Run("scheduled tick still selects by remediation priority", func(t *testing.T) {
		root, workDir := setupTargetedGatherPRContextADO(t, "run-3985-ado-schedule")
		code, stdout, stderr := runArgs(t, "gather-pr-context", root)
		if code != 0 {
			t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if got := remediationBriefSelectedNumber(t, workDir); got != "359" {
			t.Fatalf("selectedNumber = %q, want policy's pick 359 on an untargeted tick", got)
		}
	})

	t.Run("targeted run selects the dispatched pull request", func(t *testing.T) {
		root, workDir := setupTargetedGatherPRContextADO(t, "run-3985-ado-targeted")
		t.Setenv(executor.TriggerRefEnvVar, remediationTriggerRefFor(360))

		code, stdout, stderr := runArgs(t, "gather-pr-context", root)
		if code != 0 {
			t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if got := remediationBriefSelectedNumber(t, workDir); got != "360" {
			t.Fatalf("selectedNumber = %q, want the trigger's PR 360, not policy's 359", got)
		}
		if branch := strings.TrimSpace(runGitOutputT(t, workDir, "symbolic-ref", "--short", "HEAD")); branch != "goobers/impl/ado-upper" {
			t.Fatalf("checked-out branch = %q, want the targeted PR's own branch", branch)
		}
	})

	t.Run("target outside the lane's scope refuses", func(t *testing.T) {
		root, workDir := setupTargetedGatherPRContextADO(t, "run-3985-ado-refused")
		t.Setenv(executor.TriggerRefEnvVar, remediationTriggerRefFor(9999))

		code, stdout, stderr := runArgs(t, "gather-pr-context", root)
		if code != 0 {
			t.Fatalf("code = %d, stdout = %q, stderr = %q, want a clean no-work refusal", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "no work: targeted PR #9999") ||
			!strings.Contains(stdout, "is not an open pull request in this lane's selection scope") {
			t.Fatalf("stdout = %q, want a no-work naming the unselectable target", stdout)
		}
		if result := remediationBriefResult(t, workDir); result["noWork"] != true {
			t.Fatalf("result = %v, want an explicit no-work result for a refused target", result)
		}
		if branch := strings.TrimSpace(runGitOutputT(t, workDir, "symbolic-ref", "--short", "HEAD")); branch != "goobers/pr-remediation/run-3985-ado-refused" {
			t.Fatalf("checked-out branch = %q, want the run's own branch — no substitute PR was checked out", branch)
		}
	})
}
