package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func TestFindingSetDigestIsOrderIndependent(t *testing.T) {
	first := []apiv1.Finding{
		{
			Severity:    apiv1.SeverityWarning,
			Class:       apiv1.FindingCrossPRBlocked,
			Message:     "wait for siblings",
			Location:    "internal/runner/run.go:10",
			BlockingPRs: []int{12, 10, 12},
		},
		{
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingSubstantive,
			Message:  "preserve the retry result",
			Location: "cmd/goobers/applyverdict.go:1",
		},
	}
	second := []apiv1.Finding{
		first[1],
		{
			Severity:    first[0].Severity,
			Class:       first[0].Class,
			Message:     first[0].Message,
			Location:    first[0].Location,
			BlockingPRs: []int{10, 12},
		},
	}

	firstDigest, err := findingSetDigest(first)
	if err != nil {
		t.Fatalf("findingSetDigest(first): %v", err)
	}
	secondDigest, err := findingSetDigest(second)
	if err != nil {
		t.Fatalf("findingSetDigest(second): %v", err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("equivalent finding sets hashed differently: %q != %q", firstDigest, secondDigest)
	}

	second[0].Message = "a genuinely new finding"
	changedDigest, err := findingSetDigest(second)
	if err != nil {
		t.Fatalf("findingSetDigest(changed): %v", err)
	}
	if changedDigest == firstDigest {
		t.Fatalf("different finding sets shared digest %q", firstDigest)
	}
}

func TestFindingSetHistoryBootstrapsLegacyStatusAndStaysBounded(t *testing.T) {
	finding := apiv1.Finding{
		Severity: apiv1.SeverityError,
		Class:    apiv1.FindingSubstantive,
		Message:  "legacy finding",
	}
	legacy := renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{finding},
	})
	history, err := findingSetHistoryFromComments([]providers.Comment{{
		Author: "goobers",
		Body:   legacy,
	}}, "goobers")
	if err != nil {
		t.Fatalf("findingSetHistoryFromComments: %v", err)
	}
	legacyDigest, err := findingSetDigest([]apiv1.Finding{finding})
	if err != nil {
		t.Fatalf("findingSetDigest: %v", err)
	}
	if want := []string{legacyDigest}; !reflect.DeepEqual(history.Hashes, want) {
		t.Fatalf("bootstrapped history=%v, want %v", history.Hashes, want)
	}

	history.Hashes = make([]string, findingSetHistoryLimit)
	for i := range history.Hashes {
		history.Hashes[i] = "prior-" + string(rune('a'+i))
	}
	next, nextDigest, revisited, err := advanceFindingSetHistory(history, []apiv1.Finding{{
		Severity: apiv1.SeverityWarning,
		Class:    apiv1.FindingSubstantive,
		Message:  "new bounded state",
	}})
	if err != nil {
		t.Fatalf("advanceFindingSetHistory: %v", err)
	}
	if revisited {
		t.Fatal("a new finding set was reported as revisited")
	}
	if len(next.Hashes) != findingSetHistoryLimit {
		t.Fatalf("history length=%d, want bounded length %d", len(next.Hashes), findingSetHistoryLimit)
	}
	if next.Hashes[0] != "prior-b" || next.Hashes[len(next.Hashes)-1] != nextDigest {
		t.Fatalf("bounded history=%v, want oldest entry dropped and current digest appended", next.Hashes)
	}
}

func TestApplyVerdictEscalatesOnFindingSetABARevisit(t *testing.T) {
	const (
		prNumber = 367
		baseSHA  = "base-sha"
	)
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "oscillating remediation")
	files := []fakePRFile{{path: "cmd/goobers/applyverdict.go", status: "modified", additions: 3}}
	server.addOpenPR(prNumber, "goobers/implementation/oscillation", "main", "head-a-1", baseSHA, false, nil, files)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "oscillation-1")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "367")
	t.Setenv("GOOBERS_REPO_PROVIDER", "")
	t.Setenv("GOOBERS_REPO_OWNER", "")
	t.Setenv("GOOBERS_REPO_NAME", "")
	t.Chdir(t.TempDir())

	findingA := apiv1.Finding{
		Severity: apiv1.SeverityError,
		Class:    apiv1.FindingSubstantive,
		Message:  "fix A without breaking B",
		Location: "a.go:10",
	}
	findingB := apiv1.Finding{
		Severity: apiv1.SeverityError,
		Class:    apiv1.FindingSubstantive,
		Message:  "fix B without breaking A",
		Location: "b.go:20",
	}
	cycles := []struct {
		runID   string
		headSHA string
		finding apiv1.Finding
	}{
		{runID: "oscillation-1", headSHA: "head-a-1", finding: findingA},
		{runID: "oscillation-2", headSHA: "head-b-2", finding: findingB},
		{runID: "oscillation-3", headSHA: "head-a-3", finding: findingA},
	}

	for i, cycle := range cycles {
		if i > 0 {
			server.setPRHead(prNumber, cycle.headSHA, files)
			t.Setenv("GOOBERS_RUN_ID", cycle.runID)
		}
		verdict := apiv1.Verdict{
			Decision:  apiv1.VerdictNeedsChanges,
			Summary:   cycle.finding.Message,
			Rationale: "the current diff still has a substantive defect",
			Findings:  []apiv1.Finding{cycle.finding},
			HeadSHA:   cycle.headSHA,
			BaseSHA:   baseSHA,
		}
		seedGateVerdictJournal(t, root, cycle.runID, verdict)
		t.Setenv("GOOBERS_INPUT_REVIEWDIGEST", "review-"+cycle.runID)

		code, stdout, stderr := runArgs(t, "apply-verdict", root)
		if code != 0 {
			t.Fatalf("cycle %d apply-verdict: code=%d stdout=%q stderr=%q", i+1, code, stdout, stderr)
		}
		var result map[string]string
		data, err := os.ReadFile("verdict-result.json")
		if err != nil {
			t.Fatalf("cycle %d read result: %v", i+1, err)
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatalf("cycle %d unmarshal result: %v", i+1, err)
		}
		wantDecision := string(apiv1.VerdictNeedsChanges)
		if i == len(cycles)-1 {
			wantDecision = string(apiv1.VerdictFail)
		}
		if result["decision"] != wantDecision {
			t.Fatalf("cycle %d decision=%q, want %q", i+1, result["decision"], wantDecision)
		}
	}

	if !issueHasLabel(server, prNumber, remediationEscalatedLabel) {
		t.Fatal("A -> B -> A finding-set revisit did not apply goobers:merge-escalated")
	}
	if issueHasLabel(server, prNumber, needsRemediationLabel) {
		t.Fatal("oscillation escalation left goobers:needs-remediation set")
	}
	comments, _ := fakeIssueComments(t, server, prNumber)
	if len(comments) != 1 {
		t.Fatalf("comments=%q, want one sticky merge-review status comment", comments)
	}
	posted, ok := parseVerdictComment(comments[0])
	if !ok {
		t.Fatalf("final status has no verdict payload: %q", comments[0])
	}
	if posted.Decision != apiv1.VerdictFail || !strings.Contains(posted.Rationale, "Finding-set oscillation detected") {
		t.Fatalf("final verdict=%+v, want a finding-set oscillation escalation", posted)
	}
	history, ok := parseFindingSetHistoryComment(comments[0])
	if !ok {
		t.Fatalf("final status has no finding-set history: %q", comments[0])
	}
	hashA, err := findingSetDigest([]apiv1.Finding{findingA})
	if err != nil {
		t.Fatalf("hash finding A: %v", err)
	}
	hashB, err := findingSetDigest([]apiv1.Finding{findingB})
	if err != nil {
		t.Fatalf("hash finding B: %v", err)
	}
	if want := []string{hashA, hashB, hashA}; !reflect.DeepEqual(history.Hashes, want) {
		t.Fatalf("finding-set history=%v, want %v", history.Hashes, want)
	}
}
