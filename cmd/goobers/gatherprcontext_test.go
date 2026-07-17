package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// gatherPRContextServer is a small stateful fake GitHub server for
// gather-pr-context's tests: one open PR, its check state, and a fixed set of
// comments (one of which may carry an embedded verdict-json payload).
type gatherPRContextServer struct {
	owner, repo string
	prNumber    int
	head, base  string
	headSHA     string
	baseSHA     string
	checkState  string
	labels      []string
	comments    []map[string]interface{}
}

func (s gatherPRContextServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	prefix := "/repos/" + s.owner + "/" + s.repo
	mux := http.NewServeMux()

	mux.HandleFunc(prefix+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("base"); got != s.base {
			t.Fatalf("ListPullRequests base query = %q, want %q", got, s.base)
		}
		if s.prNumber == 0 {
			writeFakeJSON(w, []map[string]interface{}{})
			return
		}
		labelObjs := make([]map[string]string, len(s.labels))
		for i, l := range s.labels {
			labelObjs[i] = map[string]string{"name": l}
		}
		writeFakeJSON(w, []map[string]interface{}{
			{
				"number": s.prNumber, "draft": false,
				"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", s.owner, s.repo, s.prNumber),
				"head":     map[string]interface{}{"ref": s.head, "sha": s.headSHA},
				"base":     map[string]interface{}{"ref": s.base, "sha": s.baseSHA},
				"labels":   labelObjs,
			},
		})
	})
	mux.HandleFunc(fmt.Sprintf("%s/commits/%s/status", prefix, s.headSHA), func(w http.ResponseWriter, r *http.Request) {
		state := s.checkState
		if state == "" {
			state = "success"
		}
		writeFakeJSON(w, map[string]interface{}{
			"state": state,
			"statuses": []map[string]interface{}{
				{"context": "ci", "state": state},
			},
		})
	})
	mux.HandleFunc(fmt.Sprintf("%s/commits/%s/check-runs", prefix, s.headSHA), func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"check_runs": []map[string]interface{}{}})
	})
	mux.HandleFunc(fmt.Sprintf("%s/issues/%d/comments", prefix, s.prNumber), func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, s.comments)
	})
	mux.HandleFunc(fmt.Sprintf("%s/issues/%d", prefix, s.prNumber), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		writeFakeJSON(w, map[string]interface{}{
			"number": s.prNumber, "title": "test PR", "state": "open",
			"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", s.owner, s.repo, s.prNumber),
			"labels":   labelsJSON(s.labels),
		})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// initPRBranchOrigin builds a local bare origin (#237's no-network pattern)
// seeded with a "main" commit, an existing PR branch cut from that seed
// carrying one further commit, and THEN advances main past the point the PR
// branched from — so the PR is genuinely behind, giving
// TestGatherPRContextChecksOutSelectedPRAndLoadsContext something real to
// detect. Returns the bare origin path plus the PR head SHA and main's new
// (post-advance) tip SHA.
func initPRBranchOrigin(t *testing.T, prBranch string) (origin, headSHA, baseSHA string) {
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

	runGitT(t, work, "checkout", "-b", prBranch)
	if err := os.WriteFile(filepath.Join(work, "feature.txt"), []byte("pr work\n"), 0o644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}
	runGitT(t, work, "add", "feature.txt")
	runGitT(t, work, "commit", "-m", "pr work")
	runGitT(t, work, "push", "origin", prBranch)
	headSHA = strings.TrimSpace(runGitOutputT(t, work, "rev-parse", "HEAD"))

	runGitT(t, work, "checkout", "main")
	if err := os.WriteFile(filepath.Join(work, "unrelated.txt"), []byte("main moved on\n"), 0o644); err != nil {
		t.Fatalf("write unrelated file: %v", err)
	}
	runGitT(t, work, "add", "unrelated.txt")
	runGitT(t, work, "commit", "-m", "main moved on")
	runGitT(t, work, "push", "origin", "main")
	baseSHA = strings.TrimSpace(runGitOutputT(t, work, "rev-parse", "HEAD"))

	return origin, headSHA, baseSHA
}

// TestGatherPRContextChecksOutSelectedPRAndLoadsContext is #362's headline
// acceptance: one open PR labeled needs-remediation gets selected, its
// branch is checked out into the run's worktree (replacing the runner's own
// default branch), the base-advanced-since-branching state is detected, and
// the latest embedded verdict + full comment thread are loaded.
func TestGatherPRContextChecksOutSelectedPRAndLoadsContext(t *testing.T) {
	const prBranch = "goobers/impl/run-a"
	origin, headSHA, baseSHA := initPRBranchOrigin(t, prBranch)

	verdictComment := "**merge-review verdict: needs-changes**\n\nRebase and address one nit.\n\n" +
		`<!-- verdict-json: {"decision":"needs-changes","summary":"Rebase and address one nit.","findings":[{"severity":"warning","message":"nit","location":"PR #55","class":"substantive"}],"headSha":"` + headSHA + `","baseSha":"` + baseSHA + `"} -->`

	srv := gatherPRContextServer{
		owner: "your-org", repo: "your-repo",
		prNumber: 55, head: prBranch, base: "main",
		headSHA: headSHA, baseSHA: baseSHA,
		labels: []string{"goobers:needs-remediation"},
		comments: []map[string]interface{}{
			{"id": 1, "user": map[string]string{"login": "human-reviewer"}, "body": "please rebase", "created_at": "2026-07-01T00:00:00Z"},
			{"id": 2, "user": map[string]string{"login": "merge-review-bot"}, "body": verdictComment, "created_at": "2026-07-02T00:00:00Z"},
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
		RepoURL: origin, RunID: "run-362", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-362",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-362")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Chdir(wt.Path)

	code, stdout, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "PR #55") {
		t.Fatalf("stdout = %q, want a mention of PR #55", stdout)
	}

	branch := strings.TrimSpace(runGitOutputT(t, wt.Path, "symbolic-ref", "--short", "HEAD"))
	if branch != prBranch {
		t.Fatalf("checked-out branch = %q, want %q (the PR's own branch, not the runner's default)", branch, prBranch)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "pr-context.json"))
	if err != nil {
		t.Fatalf("read pr-context.json: %v", err)
	}
	var got struct {
		SelectedNumber         string `json:"selectedNumber"`
		Head                   string `json:"head"`
		IsBehindBase           bool   `json:"isBehindBase"`
		HasSubstantiveFindings string `json:"hasSubstantiveFindings"`
		HasFailingCI           string `json:"hasFailingCI"`
		Verdict                struct {
			Decision string `json:"decision"`
			Findings []struct {
				Class string `json:"class"`
			} `json:"findings"`
		} `json:"verdict"`
		Comments []struct {
			Author string `json:"author"`
			Body   string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal pr-context.json: %v (data=%s)", err, data)
	}
	if got.SelectedNumber != "55" || got.Head != prBranch {
		t.Fatalf("got = %+v, want selectedNumber=\"55\" head=%q", got, prBranch)
	}
	if !got.IsBehindBase {
		t.Fatal("isBehindBase = false, want true — main advanced past the PR's branch point")
	}
	if got.Verdict.Decision != "needs-changes" || len(got.Verdict.Findings) != 1 || got.Verdict.Findings[0].Class != "substantive" {
		t.Fatalf("verdict = %+v, want the embedded needs-changes verdict recovered from the comment thread", got.Verdict)
	}
	if got.HasSubstantiveFindings != "true" {
		t.Fatalf("hasSubstantiveFindings = %q, want \"true\" (the embedded verdict has a substantive finding)", got.HasSubstantiveFindings)
	}
	if got.HasFailingCI != "false" {
		t.Fatalf("hasFailingCI = %q, want \"false\"", got.HasFailingCI)
	}
	if len(got.Comments) != 2 {
		t.Fatalf("comments = %+v, want both thread comments surfaced", got.Comments)
	}
}

func TestVerdictHasSubstantiveFindingForSelectedPR(t *testing.T) {
	verdict := &apiv1.Verdict{
		Findings: []apiv1.Finding{
			{Class: apiv1.FindingRebaseNeeded, Location: "PR #485"},
			{Class: apiv1.FindingSubstantive, Location: "PR #480"},
		},
	}

	if verdictHasSubstantiveFindingForPR(verdict, 485) {
		t.Fatal("sibling PR #480's substantive finding counted for selected PR #485")
	}

	verdict.Findings = append(verdict.Findings, apiv1.Finding{
		Class:    apiv1.FindingSubstantive,
		Location: "cmd/goobers/foo.go:42",
	})
	if !verdictHasSubstantiveFindingForPR(verdict, 485) {
		t.Fatal("selected PR #485's file-scoped substantive finding was not counted")
	}

	verdict.Findings = verdict.Findings[:2]
	verdict.Findings = append(verdict.Findings, apiv1.Finding{
		Class:    apiv1.FindingSubstantive,
		Location: "PR #485",
	})
	if !verdictHasSubstantiveFindingForPR(verdict, 485) {
		t.Fatal("selected PR #485's substantive finding was not counted")
	}
}

// TestVerdictCountsCrossPRConflictFindingsForSelectedPR is #608's repro: a
// merge-review cross-PR-conflict finding points Location at the SIBLING
// ("PR #598") while its Message names what the selected PR is blocked on.
// Before the fix these were dropped as "the sibling's own issue", so
// rebase-pr reported needsAgent:false on every cycle of a genuinely
// deadlocked PR — never escalating, never converging. Finding shapes below
// are lifted verbatim from PR #597's live verdict comments.
func TestVerdictCountsCrossPRConflictFindingsForSelectedPR(t *testing.T) {
	t.Run("message names selected PR with bare #N", func(t *testing.T) {
		verdict := &apiv1.Verdict{
			Findings: []apiv1.Finding{{
				Severity: apiv1.SeverityError,
				Class:    apiv1.FindingSubstantive,
				Location: "PR #598",
				Message: "PR #598 directly rewrites the same status/runs behavior and files while converging ordering and flags. " +
					"Reconcile its shared run-table implementation with #597's runs list --json row shape and ordering.",
			}},
		}
		if !verdictHasSubstantiveFindingForPR(verdict, 597) {
			t.Fatal("cross-PR-conflict finding blocking selected PR #597 was not counted (its Location references only the sibling)")
		}
	})

	t.Run("message names selected PR with PR #N", func(t *testing.T) {
		verdict := &apiv1.Verdict{
			Findings: []apiv1.Finding{{
				Severity: apiv1.SeverityError,
				Class:    apiv1.FindingSubstantive,
				Location: "PR #538",
				Message:  "PR #538 concurrently evolves cmd/goobers/trace.go. Ensure the combined trace contract retains PR #597's JSON events.",
			}},
		}
		if !verdictHasSubstantiveFindingForPR(verdict, 597) {
			t.Fatal("cross-PR-conflict finding blocking selected PR #597 was not counted")
		}
	})

	t.Run("sibling-only finding stays excluded (#525)", func(t *testing.T) {
		verdict := &apiv1.Verdict{
			Findings: []apiv1.Finding{{
				Severity: apiv1.SeverityError,
				Class:    apiv1.FindingSubstantive,
				Location: "PR #480",
				Message:  "PR #480's new table-alignment test asserts on locale-dependent width output and fails on CI runners.",
			}},
		}
		if verdictHasSubstantiveFindingForPR(verdict, 597) {
			t.Fatal("a sibling's own substantive finding (never mentioning the selected PR) counted for selected PR #597")
		}
	})

	t.Run("sibling number in message does not count for that sibling's own gather pass", func(t *testing.T) {
		// The same live #597 finding, seen when the SELECTED PR is a
		// different, unrelated sibling (#595): neither Location nor Message
		// references #595, so it must stay excluded there.
		verdict := &apiv1.Verdict{
			Findings: []apiv1.Finding{{
				Severity: apiv1.SeverityError,
				Class:    apiv1.FindingSubstantive,
				Location: "PR #598",
				Message:  "PR #598 directly rewrites the same status/runs behavior. Reconcile its shared run-table implementation with #597's runs list --json row shape.",
			}},
		}
		if verdictHasSubstantiveFindingForPR(verdict, 595) {
			t.Fatal("a finding about the #597/#598 conflict counted for uninvolved PR #595")
		}
	})
}

// TestGatherPRContextCountsCrossPRConflictVerdict is #608's end-to-end
// acceptance: a verdict comment whose only findings name sibling PRs as the
// blocker (Location "PR #598"-style, Message "...with #597's..." — the exact
// shape merge-review posts live) must still produce
// hasSubstantiveFindings="true" for the selected PR, so rebase-pr can never
// report needsAgent:false for a verdict-confirmed deadlocked PR.
func TestGatherPRContextCountsCrossPRConflictVerdict(t *testing.T) {
	const prBranch = "goobers/impl/run-608"
	origin, headSHA, baseSHA := initPRBranchOrigin(t, prBranch)

	verdictComment := "**merge-review verdict: needs-changes**\n\nBlocked by unresolved cross-PR command-contract drift.\n\n" +
		`<!-- verdict-json: {"decision":"needs-changes","summary":"PR #597 is correct in isolation but remains blocked by unresolved cross-PR command-contract drift.","findings":[{"severity":"error","message":"PR #598 directly rewrites the same status/runs behavior and files. Reconcile its shared run-table implementation with #597's runs list --json row shape and ordering.","location":"PR #598","class":"substantive"},{"severity":"error","message":"PR #538 concurrently evolves cmd/goobers/trace.go. Ensure the combined trace JSON contract represents every transcript view exposed in text.","location":"PR #538","class":"substantive"}],"headSha":"` + headSHA + `","baseSha":"` + baseSHA + `"} -->`

	srv := gatherPRContextServer{
		owner: "your-org", repo: "your-repo",
		prNumber: 597, head: prBranch, base: "main",
		headSHA: headSHA, baseSHA: baseSHA,
		labels: []string{"goobers:needs-remediation"},
		comments: []map[string]interface{}{
			{"id": 1, "user": map[string]string{"login": "merge-review-bot"}, "body": verdictComment, "created_at": "2026-07-16T11:32:41Z"},
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
		RepoURL: origin, RunID: "run-608", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-608",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-608")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Chdir(wt.Path)

	code, stdout, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "pr-context.json"))
	if err != nil {
		t.Fatalf("read pr-context.json: %v", err)
	}
	var got struct {
		SelectedNumber         string `json:"selectedNumber"`
		HasSubstantiveFindings string `json:"hasSubstantiveFindings"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal pr-context.json: %v (data=%s)", err, data)
	}
	if got.SelectedNumber != "597" {
		t.Fatalf("selectedNumber = %q, want \"597\"", got.SelectedNumber)
	}
	if got.HasSubstantiveFindings != "true" {
		t.Fatalf("hasSubstantiveFindings = %q, want \"true\" — cross-PR-conflict findings blocking the selected PR were dropped as sibling-only (#608)", got.HasSubstantiveFindings)
	}
}

// TestSelectRemediationPRPriority is #596's headline acceptance:
// selectRemediationCandidates prioritizes needs-remediation, then failing
// CI, and only falls back to "merely behind its base" when neither
// stronger signal is present anywhere in the PR set. Unlike a single-winner
// selector, it returns every PR at the winning tier (see the "multiple
// needs-remediation PRs" case below) — claimEligiblePullRequest needs the
// whole tier to preserve exactly-once selection across concurrent runs;
// see selectRemediationCandidates' own doc comment.
func TestSelectRemediationPRPriority(t *testing.T) {
	tests := []struct {
		name         string
		prs          []providers.PullRequestSummary
		behind       map[int]bool
		wantNumbers  []int
		wantPriority remediationPriority
		wantProbes   int
	}{
		{
			name:         "behind base is fallback",
			prs:          []providers.PullRequestSummary{{Number: 12}},
			behind:       map[int]bool{12: true},
			wantNumbers:  []int{12},
			wantPriority: remediationPriorityBehindBase,
			wantProbes:   1,
		},
		{
			name: "failing CI wins over behind base",
			prs: []providers.PullRequestSummary{
				{Number: 10},
				{Number: 20, CheckState: providers.CheckStateFailing},
			},
			behind:       map[int]bool{10: true},
			wantNumbers:  []int{20},
			wantPriority: remediationPriorityFailingCI,
		},
		{
			name: "needs remediation wins over failing CI and behind base",
			prs: []providers.PullRequestSummary{
				{Number: 10},
				{Number: 20, CheckState: providers.CheckStateFailing},
				{Number: 30, Labels: []string{needsRemediationLabel}},
			},
			behind:       map[int]bool{10: true},
			wantNumbers:  []int{30},
			wantPriority: remediationPriorityNeedsRemediation,
		},
		{
			name: "multiple needs remediation PRs all returned as candidates",
			prs: []providers.PullRequestSummary{
				{Number: 40, Labels: []string{needsRemediationLabel}},
				{Number: 20, Labels: []string{needsRemediationLabel}},
			},
			wantNumbers:  []int{40, 20},
			wantPriority: remediationPriorityNeedsRemediation,
		},
		{
			name: "multiple behind-base PRs all returned as candidates",
			prs: []providers.PullRequestSummary{
				{Number: 50},
				{Number: 30},
			},
			behind:       map[int]bool{50: true, 30: true},
			wantNumbers:  []int{50, 30},
			wantPriority: remediationPriorityBehindBase,
			wantProbes:   2,
		},
		{
			// #716: escalation exclusion moved upstream of this function —
			// runGatherPRContext's self-heal-aware escalationStillBlocks
			// pre-filters prs before selectRemediationCandidates ever sees
			// them (a static label check here, unlike escalationStillBlocks,
			// couldn't tell a genuinely-still-stuck PR from one that just
			// self-healed but hasn't had its label cleared yet). This table
			// pins the resulting contract: a labeled PR that reaches this
			// function is treated like any other — the label alone is not
			// this layer's concern. See TestGatherPRContextExcludesEscalated
			// NeedsRemediationPR/escalationlivelock716_test.go for the actual
			// exclusion behavior, tested at the layer that owns it now.
			name: "labeled PR reaching this layer is not itself excluded",
			prs: []providers.PullRequestSummary{
				{Number: 10, Labels: []string{remediationEscalatedLabel}},
				{Number: 20},
			},
			behind:       map[int]bool{10: true, 20: true},
			wantNumbers:  []int{10, 20},
			wantPriority: remediationPriorityBehindBase,
			wantProbes:   2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probes := 0
			candidates, priority, err := selectRemediationCandidates(tt.prs, func(pr providers.PullRequestSummary) (bool, error) {
				probes++
				return tt.behind[pr.Number], nil
			})
			if err != nil {
				t.Fatalf("selectRemediationCandidates: %v", err)
			}
			if priority != tt.wantPriority {
				t.Fatalf("priority = %d, want %d", priority, tt.wantPriority)
			}
			gotNumbers := make([]int, len(candidates))
			for i, c := range candidates {
				gotNumbers[i] = c.Number
			}
			if len(gotNumbers) != len(tt.wantNumbers) {
				t.Fatalf("candidates = %v, want %v", gotNumbers, tt.wantNumbers)
			}
			for i, want := range tt.wantNumbers {
				if gotNumbers[i] != want {
					t.Fatalf("candidates = %v, want %v", gotNumbers, tt.wantNumbers)
				}
			}
			if probes != tt.wantProbes {
				t.Fatalf("behind-base probes = %d, want %d", probes, tt.wantProbes)
			}
		})
	}
}

// TestSelectRemediationCandidatesNoneEligible proves an empty PR set (or a
// set where nothing clears any tier) reports remediationPriorityNone with
// no candidates, rather than a spurious behind-base match.
func TestSelectRemediationCandidatesNoneEligible(t *testing.T) {
	candidates, priority, err := selectRemediationCandidates(nil, func(providers.PullRequestSummary) (bool, error) {
		t.Fatal("behindBase probe should not run against an empty PR set")
		return false, nil
	})
	if err != nil {
		t.Fatalf("selectRemediationCandidates: %v", err)
	}
	if len(candidates) != 0 || priority != remediationPriorityNone {
		t.Fatalf("candidates = %v, priority = %d, want none", candidates, priority)
	}
}

func TestGatherPRContextSelectsUnlabeledFailingPR(t *testing.T) {
	const prBranch = "goobers/impl/run-ci-red"
	origin, headSHA, baseSHA := initPRBranchOrigin(t, prBranch)

	srv := gatherPRContextServer{
		owner: "your-org", repo: "your-repo",
		prNumber: 56, head: prBranch, base: "main",
		headSHA: headSHA, baseSHA: baseSHA,
		checkState: "failure",
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
		RepoURL: origin, RunID: "run-ci-red", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-ci-red",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-ci-red")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Chdir(wt.Path)

	code, stdout, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "PR #56") {
		t.Fatalf("stdout = %q, want a mention of PR #56", stdout)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "pr-context.json"))
	if err != nil {
		t.Fatalf("read pr-context.json: %v", err)
	}
	var got struct {
		SelectedNumber string `json:"selectedNumber"`
		Head           string `json:"head"`
		HasFailingCI   string `json:"hasFailingCI"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal pr-context.json: %v (data=%s)", err, data)
	}
	if got.SelectedNumber != "56" || got.Head != prBranch {
		t.Fatalf("got = %+v, want selectedNumber=\"56\" head=%q", got, prBranch)
	}
	if got.HasFailingCI != "true" {
		t.Fatalf("hasFailingCI = %q, want \"true\"", got.HasFailingCI)
	}
}

func TestGatherPRContextSelectsBehindBaseOnlyPRAndRebases(t *testing.T) {
	const prBranch = "goobers/impl/run-behind"
	origin, headSHA, baseSHA := initPRBranchOrigin(t, prBranch)

	srv := gatherPRContextServer{
		owner: "your-org", repo: "your-repo",
		prNumber: 58, head: prBranch, base: "main",
		headSHA: headSHA, baseSHA: baseSHA,
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
		RepoURL: origin, RunID: "run-behind", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-behind",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-behind")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Chdir(wt.Path)

	code, stdout, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 0 {
		t.Fatalf("gather-pr-context code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "pr-context.json"))
	if err != nil {
		t.Fatalf("read pr-context.json: %v", err)
	}
	var contextResult struct {
		SelectedNumber         string `json:"selectedNumber"`
		Head                   string `json:"head"`
		Base                   string `json:"base"`
		HasSubstantiveFindings string `json:"hasSubstantiveFindings"`
		HasFailingCI           string `json:"hasFailingCI"`
	}
	if err := json.Unmarshal(data, &contextResult); err != nil {
		t.Fatalf("unmarshal pr-context.json: %v", err)
	}
	if contextResult.SelectedNumber != "58" {
		t.Fatalf("selectedNumber = %q, want 58", contextResult.SelectedNumber)
	}

	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", contextResult.SelectedNumber)
	t.Setenv("GOOBERS_INPUT_HEAD", contextResult.Head)
	t.Setenv("GOOBERS_INPUT_BASE", contextResult.Base)
	t.Setenv("GOOBERS_INPUT_HASSUBSTANTIVEFINDINGS", contextResult.HasSubstantiveFindings)
	t.Setenv("GOOBERS_INPUT_HASFAILINGCI", contextResult.HasFailingCI)
	code, stdout, stderr = runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("rebase-pr code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	verify := t.TempDir()
	runGitT(t, verify, "clone", "--branch", prBranch, origin, filepath.Join(verify, "check"))
	if _, err := os.Stat(filepath.Join(verify, "check", "unrelated.txt")); err != nil {
		t.Fatalf("origin's %s branch was not rebased onto advanced main: %v", prBranch, err)
	}
}

func TestGatherPRContextDoesNotReselectEscalatedFailingPR(t *testing.T) {
	srv := gatherPRContextServer{
		owner: "your-org", repo: "your-repo",
		prNumber: 57, head: "goobers/impl/escalated", base: "main",
		headSHA: "deadbeef", baseSHA: "cafebabe",
		checkState: "failure",
		labels:     []string{remediationEscalatedLabel},
	}
	server := srv.start(t)

	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: server.URL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-ci-red-escalated")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want no work after terminal escalation", stdout)
	}
}

// TestGatherPRContextNoEligiblePRIsNoWork proves gather-pr-context succeeds
// (exit 0, no-work) rather than erroring when no PR is labeled or failing —
// a normal outcome (mirrors pr-select's own no-work shape), not an error.
func TestGatherPRContextNoEligiblePRIsNoWork(t *testing.T) {
	srv := gatherPRContextServer{owner: "your-org", repo: "your-repo", base: "main"}
	server := srv.start(t)

	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: server.URL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-362-empty")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q, want 0 (no-work)", code, stderr)
	}
}

// TestGatherPRContextRefusesWithoutCapability proves gather-pr-context fails
// closed before any provider/git call when a required capability is absent.
func TestGatherPRContextRefusesWithoutCapability(t *testing.T) {
	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-362-nocap")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	// Deliberately no GOOBERS_CRED_* set.
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want 1 (fail closed on missing capability)", code, stderr)
	}
}
