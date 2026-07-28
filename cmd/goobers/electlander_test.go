package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestElectedLander is #833's election policy in isolation: lowest PR number
// wins its cluster — thisPR is the lander iff its number is below every PR it
// is blocked on.
func TestElectedLander(t *testing.T) {
	tests := []struct {
		name     string
		thisPR   int
		blockers []int
		want     bool
	}{
		{"lowest of the cluster wins", 810, []int{811, 812}, true},
		{"a higher member parks (a lower blocker exists)", 811, []int{810, 812}, false},
		{"highest member parks", 812, []int{810, 811}, false},
		{"no named blockers trivially wins", 810, nil, true},
		{"single higher blocker wins", 810, []int{999}, true},
		{"single lower blocker parks", 999, []int{810}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := electedLander(tt.thisPR, tt.blockers); got != tt.want {
				t.Fatalf("electedLander(%d, %v) = %v, want %v", tt.thisPR, tt.blockers, got, tt.want)
			}
		})
	}
}

// TestElectionDecision is the composite gate the elect-lander stage applies:
// election fires only for a verdict that is ENTIRELY cross-PR-ordering asks
// (the PR is individually fine, merely sibling-blocked) AND wins its cluster.
// Any real defect makes the PR non-electable regardless of numbering.
func TestElectionDecision(t *testing.T) {
	crossPR := func(blockers ...int) apiv1.Finding {
		return apiv1.Finding{Class: apiv1.FindingCrossPRBlocked, BlockingPRs: blockers}
	}
	substantive := apiv1.Finding{Class: apiv1.FindingSubstantive}
	// #1726: an `info` nit — e.g. "this generated patch is byte-for-byte
	// identical to the selected PR's, there is no semantic conflict" — is not a
	// reason to withhold landing authority.
	infoNit := apiv1.Finding{Class: apiv1.FindingSubstantive, Severity: apiv1.SeverityInfo}
	warningDefect := apiv1.Finding{Class: apiv1.FindingSubstantive, Severity: apiv1.SeverityWarning}
	errorDefect := apiv1.Finding{Class: apiv1.FindingSubstantive, Severity: apiv1.SeverityError}

	tests := []struct {
		name     string
		findings []apiv1.Finding
		thisPR   int
		want     bool
	}{
		{"all cross-pr, lowest -> elected", []apiv1.Finding{crossPR(811)}, 810, true},
		{"all cross-pr, not lowest -> parked", []apiv1.Finding{crossPR(810)}, 811, false},
		{"cross-pr + a real defect -> not electable", []apiv1.Finding{crossPR(811), substantive}, 810, false},
		{"empty findings -> not electable", nil, 810, false},
		{"multiple cross-pr findings, lowest overall -> elected", []apiv1.Finding{crossPR(811), crossPR(812)}, 810, true},
		{"multiple cross-pr findings, a lower one present -> parked", []apiv1.Finding{crossPR(812), crossPR(809)}, 810, false},
		// #1726: the live deadlock — winner carried only info-severity findings.
		{"cross-pr + info nit -> still electable", []apiv1.Finding{crossPR(811), infoNit}, 810, true},
		{"cross-pr + several info nits -> still electable", []apiv1.Finding{crossPR(811), infoNit, infoNit, infoNit}, 810, true},
		{"cross-pr + warning defect -> not electable", []apiv1.Finding{crossPR(811), warningDefect}, 810, false},
		{"cross-pr + error defect -> not electable", []apiv1.Finding{crossPR(811), errorDefect}, 810, false},
		// An unset severity must keep failing closed: a malformed verdict cannot
		// launder itself into landing authority.
		{"cross-pr + unset-severity defect -> not electable", []apiv1.Finding{crossPR(811), substantive}, 810, false},
		// Info alone is not an ordering finding, so there is no cluster to win.
		{"info nit only, no ordering finding -> not electable", []apiv1.Finding{infoNit}, 810, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := electionDecision(tt.findings, tt.thisPR, electedLander, nil); got != tt.want {
				t.Fatalf("electionDecision(%v, %d) = %v, want %v", tt.findings, tt.thisPR, got, tt.want)
			}
		})
	}
}

func TestNoLanderEscalationReason(t *testing.T) {
	substantive := []apiv1.Finding{{Class: apiv1.FindingSubstantive}}
	crossPR := []apiv1.Finding{{Class: apiv1.FindingCrossPRBlocked, BlockingPRs: []int{11}}}

	tests := []struct {
		name       string
		decision   apiv1.VerdictDecision
		findings   []apiv1.Finding
		selected   int
		overlaps   []int
		demoted    map[int]bool
		wantReason bool
	}{
		{"policy winner with real defect has no lander", apiv1.VerdictNeedsChanges, substantive, 10, []int{11}, nil, true},
		{"non-winner with real defect leaves the winner available", apiv1.VerdictNeedsChanges, substantive, 11, []int{10}, nil, false},
		{"pure ordering winner is electable", apiv1.VerdictNeedsChanges, crossPR, 10, []int{11}, nil, false},
		{"no deterministic overlap is not a cluster", apiv1.VerdictNeedsChanges, substantive, 10, nil, nil, false},
		{"demoted winner yields to the next candidate", apiv1.VerdictNeedsChanges, substantive, 10, []int{11}, map[int]bool{10: true}, false},
		{"defective winner with all siblings demoted still has no lander", apiv1.VerdictNeedsChanges, substantive, 11, []int{10}, map[int]bool{10: true}, true},
		{"fail verdict is already an explicit escalation", apiv1.VerdictFail, substantive, 10, []int{11}, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := noLanderEscalationReason(tt.decision, tt.findings, tt.selected, tt.overlaps, electedLander, tt.demoted, "fifo")
			if (reason != "") != tt.wantReason {
				t.Fatalf("noLanderEscalationReason() = %q, wantReason %v", reason, tt.wantReason)
			}
			if tt.wantReason {
				for _, want := range []string{"Cluster has no lander", "#10", "#11", "fifo"} {
					if !strings.Contains(reason, want) {
						t.Errorf("reason = %q, want it to contain %q", reason, want)
					}
				}
			}
		})
	}
}

func TestAsymmetricFindingsEscalateClusterWithoutLander(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")

	const (
		selectedNumber = 10
		siblingNumber  = 11
		runID          = "asymmetric-lander"
	)
	server.addIssue(selectedNumber, "deterministic winner")
	server.addIssue(siblingNumber, "blocked sibling", blockedOnSiblingLabel)
	overlap := []fakePRFile{{path: "cmd/goobers/electlander.go", status: "modified", additions: 1}}
	server.addOpenPR(selectedNumber, "goobers/implementation/10", "main", "head-10", "base", false, nil, overlap)
	server.addOpenPR(siblingNumber, "goobers/implementation/11", "main", "head-11", "base", false, []string{blockedOnSiblingLabel}, overlap)
	blockedComment, err := blockedOnSiblingComment(blockedOnSiblingState{
		Blockers: []int{selectedNumber},
		Reason:   "reviewer classified the overlap as cross-pr-blocked",
		HeadSHA:  "head-11",
		BaseSHA:  "base",
	})
	if err != nil {
		t.Fatalf("build sibling blocked record: %v", err)
	}
	server.addComment(siblingNumber, blockedComment)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")
	t.Setenv("GOOBERS_INPUT_OVERLAPPINGSIBLINGS", "11")
	t.Setenv("GOOBERS_INPUT_SCOPEGATEPARKED", "true")
	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision:  apiv1.VerdictNeedsChanges,
		Rationale: "the overlap is a substantive conflict",
		HeadSHA:   "head-10",
		BaseSHA:   "base",
		Findings: []apiv1.Finding{{
			Class:    apiv1.FindingSubstantive,
			Severity: apiv1.SeverityError,
			Message:  "the sibling changed an assumption this PR relies on",
		}},
	})

	electionDir := t.TempDir()
	t.Chdir(electionDir)
	code, stdout, stderr := runArgs(t, "elect-lander", root)
	if code != 0 {
		t.Fatalf("elect-lander: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Cluster has no lander") {
		t.Fatalf("elect-lander stdout = %q, want explicit no-lander decision", stdout)
	}
	data, err := os.ReadFile(filepath.Join(electionDir, "election.json"))
	if err != nil {
		t.Fatalf("read election result: %v", err)
	}
	var election map[string]string
	if err := json.Unmarshal(data, &election); err != nil {
		t.Fatalf("unmarshal election result: %v", err)
	}
	if election["elected"] != "false" {
		t.Fatalf("elected = %q, want false", election["elected"])
	}
	if election["scopeGateParked"] != "true" {
		t.Fatalf("scopeGateParked = %q, want true", election["scopeGateParked"])
	}

	applyDir := t.TempDir()
	t.Chdir(applyDir)
	code, stdout, stderr = runArgs(t, "apply-verdict", root)
	if code != 0 {
		t.Fatalf("apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err = os.ReadFile(filepath.Join(applyDir, "verdict-result.json"))
	if err != nil {
		t.Fatalf("read apply-verdict result: %v", err)
	}
	var applied map[string]string
	if err := json.Unmarshal(data, &applied); err != nil {
		t.Fatalf("unmarshal apply-verdict result: %v", err)
	}
	if applied["decision"] != string(apiv1.VerdictFail) {
		t.Fatalf("applied decision = %q, want fail", applied["decision"])
	}
	if applied["scopeGateParked"] != "true" {
		t.Fatalf("applied scopeGateParked = %q, want true", applied["scopeGateParked"])
	}

	server.mu.Lock()
	labels := append([]string(nil), server.issues[selectedNumber].labels...)
	comments := append([]string(nil), server.issues[selectedNumber].comments...)
	server.mu.Unlock()
	if len(comments) != 1 {
		t.Fatalf("comments = %v, want one escalation comment", comments)
	}
	hasEscalationLabel := false
	for _, label := range labels {
		if label == "goobers:merge-escalated" {
			hasEscalationLabel = true
			break
		}
	}
	if !hasEscalationLabel {
		t.Fatalf("labels = %v, want goobers:merge-escalated (apply-verdict stdout = %q)", labels, stdout)
	}
	for _, want := range []string{"Cluster has no lander", "#10", "#11", "fifo"} {
		if !strings.Contains(comments[0], want) {
			t.Errorf("escalation comment = %q, want it to contain %q", comments[0], want)
		}
	}
}

func TestParkedClusterMemberQueuesCrownedLanderPriorityDispatch(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")

	const (
		landerNumber   = 10
		selectedNumber = 11
		runID          = "priority-lander"
	)
	overlap := []fakePRFile{{path: "cmd/goobers/electlander.go", status: "modified", additions: 1}}
	server.addIssue(landerNumber, "crowned lander")
	server.addIssue(selectedNumber, "later cluster member")
	server.addOpenPR(landerNumber, "goobers/implementation/10", "main", "head-10", "base", false, nil, overlap)
	server.addOpenPR(selectedNumber, "goobers/implementation/11", "main", "head-11", "base", false, nil, overlap)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "11")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", "head-11")
	t.Setenv("GOOBERS_INPUT_SELECTEDBASESHA", "base")
	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision:  apiv1.VerdictNeedsChanges,
		Rationale: "PR #10 must land first",
		HeadSHA:   "head-11",
		BaseSHA:   "base",
		Findings: []apiv1.Finding{{
			Class:       apiv1.FindingCrossPRBlocked,
			Severity:    apiv1.SeverityInfo,
			Message:     "waiting for the elected predecessor",
			BlockingPRs: []int{landerNumber},
		}},
	})

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "apply-verdict", root)
	if code != 0 {
		t.Fatalf("apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "queued an immediate merge-review re-tick") {
		t.Fatalf("stdout = %q, want priority re-tick confirmation", stdout)
	}

	data, err := os.ReadFile(filepath.Join(workDir, "verdict-result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result["priorityDispatchRequested"] != "true" {
		t.Fatalf("priorityDispatchRequested = %q, want true", result["priorityDispatchRequested"])
	}

	requests, err := filepath.Glob(filepath.Join(layoutFor(root).SchedulerDir(), pendingTriggersDir, "*"+requestSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("priority trigger requests = %v, want one", requests)
	}
	requestData, err := os.ReadFile(requests[0])
	if err != nil {
		t.Fatal(err)
	}
	var request triggerRequest
	if err := json.Unmarshal(requestData, &request); err != nil {
		t.Fatal(err)
	}
	if !request.Priority || request.Gaggle != "goobers" || request.Workflow != "merge-review" || request.SourceRun != runID {
		t.Fatalf("priority trigger request = %+v", request)
	}
}

// TestElectedNewest is #834's second built-in policy: highest PR number wins.
func TestElectedNewest(t *testing.T) {
	tests := []struct {
		name     string
		thisPR   int
		blockers []int
		want     bool
	}{
		{"highest of the cluster wins", 812, []int{810, 811}, true},
		{"a lower member parks (a higher blocker exists)", 811, []int{810, 812}, false},
		{"lowest member parks", 810, []int{811, 812}, false},
		{"no named blockers trivially wins", 812, nil, true},
		{"single lower blocker wins", 999, []int{810}, true},
		{"single higher blocker parks", 810, []int{999}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := electedNewest(tt.thisPR, tt.blockers); got != tt.want {
				t.Fatalf("electedNewest(%d, %v) = %v, want %v", tt.thisPR, tt.blockers, got, tt.want)
			}
		})
	}
}

// TestResolveElectionPolicy is #834's config resolution: known names resolve to
// their policy; an unknown/empty name falls back to the deterministic default
// (fifo) rather than failing, and reports the fallback name so the stage can
// log it. Verified behaviorally (fifo lowest-wins vs newest highest-wins) since
// funcs are not comparable.
func TestResolveElectionPolicy(t *testing.T) {
	tests := []struct {
		name        string
		policyName  string
		wantName    string
		thisPR      int
		blockers    []int
		wantElected bool // fifo: lowest wins; newest: highest wins
	}{
		{"fifo resolves and elects lowest", "fifo", "fifo", 810, []int{811}, true},
		{"newest resolves and elects highest", "newest", "newest", 811, []int{810}, true},
		{"unknown falls back to fifo", "bogus", "fifo", 810, []int{811}, true},
		{"empty falls back to fifo", "", "fifo", 810, []int{811}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, gotName := resolveElectionPolicy(tt.policyName)
			if gotName != tt.wantName {
				t.Fatalf("resolveElectionPolicy(%q) name = %q, want %q", tt.policyName, gotName, tt.wantName)
			}
			if got := policy(tt.thisPR, tt.blockers); got != tt.wantElected {
				t.Fatalf("resolved policy %q elected(%d, %v) = %v, want %v", gotName, tt.thisPR, tt.blockers, got, tt.wantElected)
			}
		})
	}
}

// TestInfoFindingsDoNotDeadlockCluster covers the #1726 deadlock end to end at
// the predicate level: the winner's review carries only `info` findings that
// each say in their own text that there is no conflict, so it must be crowned
// and no human-intervention escalation must be emitted.
//
// Both gates have to agree for this to work. withOverlapBackstop must still
// synthesise the ordering finding (an info nit used to suppress it, leaving no
// cross-pr-blocked finding to elect on), and electionDecision must then crown
// the winner. Fixing only one leaves the cluster stalled.
func TestInfoFindingsDoNotDeadlockCluster(t *testing.T) {
	infoNit := func(msg string) apiv1.Finding {
		return apiv1.Finding{Class: apiv1.FindingSubstantive, Severity: apiv1.SeverityInfo, Message: msg}
	}
	// The live cluster: #1717 the winner, siblings #1719/#1723/#1724, all three
	// findings identical-generated-churn notes.
	findings := []apiv1.Finding{
		infoNit("zz_generated.deepcopy.go patch is byte-for-byte identical; there is no semantic conflict"),
		infoNit("overlaps only in separate Windows/WSL guidance strings"),
		infoNit("fan-in implementation is otherwise in a separate subsystem"),
	}
	siblings := []int{1719, 1723, 1724}

	backstopped := withOverlapBackstop(findings, siblings)
	if len(backstopped) != len(findings)+1 {
		t.Fatalf("withOverlapBackstop returned %d findings, want %d — info nits must not suppress the ordering backstop",
			len(backstopped), len(findings)+1)
	}

	if !electionDecision(backstopped, 1717, electedLander, nil) {
		t.Fatal("winner carrying only info findings was not crowned — the cluster deadlocks")
	}

	if reason := noLanderEscalationReason(
		apiv1.VerdictNeedsChanges, backstopped, 1717, siblings, electedLander, nil, "fifo",
	); reason != "" {
		t.Fatalf("emitted a human-intervention escalation for a crownable winner: %s", reason)
	}
}

// TestRealDefectStillBlocksCrowning is the guard against over-reach: #1719 in
// the same live cluster carried genuine error-severity findings and was
// correctly left in remediation. Severity-awareness must not launder that.
func TestRealDefectStillBlocksCrowning(t *testing.T) {
	findings := []apiv1.Finding{
		{Class: apiv1.FindingSubstantive, Severity: apiv1.SeverityInfo, Message: "identical generated churn"},
		{Class: apiv1.FindingSubstantive, Severity: apiv1.SeverityError, Message: "mid-parallel resume parity"},
	}
	// The defect sits on the deterministic winner (lowest number under fifo),
	// which is the only shape that produces the asymmetric no-lander state.
	const winner = 1717
	siblings := []int{1719, 1723, 1724}

	if got := withOverlapBackstop(findings, siblings); len(got) != len(findings) {
		t.Fatalf("withOverlapBackstop appended to findings carrying a real defect (%d -> %d)", len(findings), len(got))
	}
	if electionDecision(findings, winner, electedLander, nil) {
		t.Fatal("crowned a PR carrying an error-severity finding")
	}
	if reason := noLanderEscalationReason(
		apiv1.VerdictNeedsChanges, findings, winner, siblings, electedLander, nil, "fifo",
	); reason == "" {
		t.Fatal("no escalation for a genuine no-lander state — the asymmetric case must still surface")
	}
}
