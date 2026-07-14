package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// readMutationFacts reads and parses every line of mutations.jsonl under
// dir, the sidecar cmd/goobers's provider-chain subcommands write for the
// runner to project into ref.touched events (issue #228).
func readMutationFacts(t *testing.T, dir string) []mutationFact {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, mutationsSidecarFile))
	if err != nil {
		t.Fatalf("read %s: %v", mutationsSidecarFile, err)
	}
	var facts []mutationFact
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var f mutationFact
		if err := json.Unmarshal(line, &f); err != nil {
			t.Fatalf("unmarshal mutation fact %q: %v", line, err)
		}
		facts = append(facts, f)
	}
	return facts
}

// TestOpenPRWritesMutationSidecar is issue #228's negative-control test for
// the "pr" kind: a real `goobers open-pr` invocation (the actual CLI
// entrypoint, not just providers' own unit tests) against a fake provider
// leaves a mutations.jsonl fact the runner projects into ref.touched.
func TestOpenPRWritesMutationSidecar(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "open-pr", root)
	if code != 0 {
		t.Fatalf("open-pr: code = %d, stderr = %q", code, stderr)
	}

	facts := readMutationFacts(t, workDir)
	if len(facts) != 1 {
		t.Fatalf("mutation facts = %#v, want exactly 1", facts)
	}
	f := facts[0]
	if f.Provider != "github" || f.Kind != "pr" || f.ID != "1" || f.Operation != "open" || f.URL == "" {
		t.Fatalf("unexpected mutation fact: %+v", f)
	}
}

// TestBacklogQueryClaimWritesMutationSidecar is issue #228's negative-control
// test for the "issue" kind via backlog-query --claim: the claim marker
// mutation (providers' own UpdateWorkItemStatus "status" operation) also
// leaves a mutations.jsonl fact.
func TestBacklogQueryClaimWritesMutationSidecar(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	facts := readMutationFacts(t, workDir)
	if len(facts) != 1 {
		t.Fatalf("mutation facts = %#v, want exactly 1", facts)
	}
	f := facts[0]
	if f.Provider != "github" || f.Kind != "issue" || f.ID != "7" {
		t.Fatalf("unexpected mutation fact: %+v", f)
	}
}

// TestIssueCloseOutWritesMutationSidecar is issue #228's negative-control
// test for issue-close-out.
func TestIssueCloseOutWritesMutationSidecar(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	const runID = "run-1"
	const workflow = "implementation"

	schedulerDir := filepath.Join(root, "scheduler")
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Claim("7", runID, workflow, time.Hour); err != nil {
		t.Fatal(err)
	}

	head := providers.BranchName(workflow, runID)
	server.mu.Lock()
	server.prs[1] = &fakePR{number: 1, title: "Implementation", head: head, base: "main", state: "open"}
	server.nextPR = 2
	server.mu.Unlock()

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", runID)
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "issue-close-out", root)
	if code != 0 {
		t.Fatalf("issue-close-out: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "closed out 7") {
		t.Fatalf("stdout = %q, want a mention of the closed-out item", stdout)
	}

	facts := readMutationFacts(t, workDir)
	if len(facts) == 0 {
		t.Fatal("expected at least one mutation fact (comment and/or status close)")
	}
	for _, f := range facts {
		if f.Provider != "github" || f.Kind != "issue" || f.ID != "7" {
			t.Fatalf("unexpected mutation fact: %+v", f)
		}
	}
}
