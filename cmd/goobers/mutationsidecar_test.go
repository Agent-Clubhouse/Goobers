package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func TestMutationSidecarPreservesQueueAdmissionAcrossWireConsumers(t *testing.T) {
	t.Chdir(t.TempDir())
	admission := &providers.QueueAdmission{IntentID: "0123456789abcdef0123456789abcdef", RepositoryAPIURL: "https://forge.example/team/repos/acme/app", PullID: "9", EntryID: "MQE_owned", ExpectedHeadSHA: "head", EnqueuedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	sidecarMutationRecorder{kind: "pr"}.RecordExternalRef(context.Background(), providers.ExternalRef{Provider: providers.ProviderGitHub, Ref: "acme/app#9", Operation: "enqueue", QueueAdmission: admission})
	data, err := os.ReadFile(mutationsSidecarFile)
	if err != nil {
		t.Fatal(err)
	}
	var local mutationFact
	var remote dispatcher.SurrenderedMutation
	var temporal engine.MutationFact
	for _, target := range []any{&local, &remote, &temporal} {
		if err := json.Unmarshal(data, target); err != nil {
			t.Fatal(err)
		}
	}
	if local.ReceiptID == "" || remote.ReceiptID != local.ReceiptID || temporal.ReceiptID != local.ReceiptID {
		t.Fatal("durable receipt identity lost in wire transport")
	}
	for _, got := range []*providers.QueueAdmission{local.QueueAdmission, remote.QueueAdmission, temporal.QueueAdmission} {
		if got == nil || *got != *admission {
			t.Fatalf("queue receipt lost in sidecar transport: %+v", got)
		}
	}
	if local.MergeConfirmation != nil || remote.MergeConfirmation != nil || temporal.MergeConfirmation != nil || local.Operation != "enqueue" {
		t.Fatal("queue acceptance promoted to merge confirmation")
	}
}

func TestIdenticalMutationRecordsGetDistinctDurableIdentities(t *testing.T) {
	t.Chdir(t.TempDir())
	fact := mutationFact{Provider: "github", Kind: "pr", ID: "9", Operation: "merge"}
	for i := 0; i < 2; i++ {
		if err := appendMutationFact(fact); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(mutationsSidecarFile)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var first, second mutationFact
	if err := decoder.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&second); err != nil {
		t.Fatal(err)
	}
	if first.ReceiptID == "" || second.ReceiptID == "" || first.ReceiptID == second.ReceiptID {
		t.Fatal("different durable records share an identity")
	}
}

func TestMutationSidecarPreservesLandingIntentAcrossWireConsumers(t *testing.T) {
	t.Chdir(t.TempDir())
	intent := providers.LandingIntent{ID: "0123456789abcdef0123456789abcdef", Operation: "enqueue", RepositoryAPIURL: "https://forge.example/repos/acme/app", PullID: "9", ExpectedHeadSHA: "expected"}
	if err := (sidecarMutationRecorder{kind: "pr"}).RecordLandingIntent(context.Background(), providers.ProviderGitHub, intent); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(mutationsSidecarFile)
	if err != nil {
		t.Fatal(err)
	}
	var local mutationFact
	var remote dispatcher.SurrenderedMutation
	var temporal engine.MutationFact
	for _, target := range []any{&local, &remote, &temporal} {
		if err := json.Unmarshal(data, target); err != nil {
			t.Fatal(err)
		}
	}
	for _, got := range []*providers.LandingIntent{local.LandingIntent, remote.LandingIntent, temporal.LandingIntent} {
		if got == nil || *got != intent {
			t.Fatalf("lost landing intent: %+v", got)
		}
	}
	if local.Operation != "merge-intent" || local.MergeConfirmation != nil {
		t.Fatalf("attempt promoted to completed merge: %+v", local)
	}
}

func TestMutationSidecarPreservesAcknowledgedAutoCompleteIntent(t *testing.T) {
	t.Chdir(t.TempDir())
	intent := &providers.LandingIntent{ID: "0123456789abcdef0123456789abcdef", Operation: "enqueue", RepositoryAPIURL: "https://dev.azure.com/org/project/_apis/git/repositories/repo", PullID: "42", ExpectedHeadSHA: "head"}
	sidecarMutationRecorder{kind: "pr"}.RecordExternalRef(context.Background(), providers.ExternalRef{
		Provider: providers.ProviderADO, Ref: "ado#42", Operation: "enqueue", LandingIntent: intent,
	})
	data, err := os.ReadFile(mutationsSidecarFile)
	if err != nil {
		t.Fatal(err)
	}
	var local mutationFact
	var remote dispatcher.SurrenderedMutation
	var temporal engine.MutationFact
	for _, target := range []any{&local, &remote, &temporal} {
		if err := json.Unmarshal(data, target); err != nil {
			t.Fatal(err)
		}
	}
	for _, got := range []*providers.LandingIntent{local.LandingIntent, remote.LandingIntent, temporal.LandingIntent} {
		if got == nil || *got != *intent {
			t.Fatalf("acknowledgement lost intent: %+v", got)
		}
	}
	if local.Operation != "enqueue" || local.ID != "42" || local.MergeConfirmation != nil || local.QueueAdmission != nil || remote.MergeConfirmation != nil || remote.QueueAdmission != nil || temporal.MergeConfirmation != nil || temporal.QueueAdmission != nil {
		t.Fatalf("acknowledgement promoted to merge or queue entry: %+v", local)
	}
}

func TestLandingIntentSidecarFailureIsReturned(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir(mutationsSidecarFile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := (sidecarMutationRecorder{kind: "pr"}).RecordLandingIntent(context.Background(), providers.ProviderGitHub, providers.LandingIntent{}); err == nil {
		t.Fatal("intent persistence error was swallowed")
	}
}

func TestMutationSidecarPreservesMergeConfirmationAcrossWireConsumers(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	confirmation := &providers.MergeConfirmation{
		RepositoryAPIURL: "https://forge.example/team/repos/acme/app", PullID: "9", MergeSHA: "commit",
	}
	sidecarMutationRecorder{kind: "pr"}.RecordExternalRef(context.Background(), providers.ExternalRef{
		Provider: providers.ProviderGitHub, Ref: "acme/app#9", Operation: "merge", MergeConfirmation: confirmation,
	})
	facts := readMutationFacts(t, dir)
	if len(facts) != 1 || facts[0].MergeConfirmation == nil || *facts[0].MergeConfirmation != *confirmation {
		t.Fatalf("sidecar dropped merge evidence: %+v", facts)
	}
	data, err := os.ReadFile(mutationsSidecarFile)
	if err != nil {
		t.Fatal(err)
	}
	var activity engine.MutationFact
	var pod dispatcher.SurrenderedMutation
	for _, value := range []any{&activity, &pod} {
		if err := json.Unmarshal(data, value); err != nil {
			t.Fatal(err)
		}
	}
	if activity.MergeConfirmation == nil || pod.MergeConfirmation == nil || *activity.MergeConfirmation != *confirmation || *pod.MergeConfirmation != *confirmation {
		t.Fatalf("transport dropped confirmation: activity=%+v pod=%+v", activity, pod)
	}
}

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

	providerCmdEnv(t, server, "GOOBERS_CRED_PROVIDER_PR_WRITE", "run-1")
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
	for _, operation := range []string{"comment", "claim"} {
		found := false
		for _, fact := range facts {
			if fact.Provider == "github" && fact.Kind == "issue" &&
				fact.ID == "7" && fact.Operation == operation {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("mutation facts = %#v, want github issue 7 %s mutation", facts, operation)
		}
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

// TestRecordExternalRefLogsOnOpenFailure is #2029's writer-side regression:
// RecordExternalRef must never fail the mutation the provider already made
// for real, but a failed write must still be observable via a log line —
// the only channel available to this short-lived subprocess, which has no
// legal journal access (see mutationsSidecarFile's doc).
func TestRecordExternalRefLogsOnOpenFailure(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	// Pre-create the sidecar path as a directory so OpenFile(O_WRONLY) on
	// it fails deterministically, without needing a read-only filesystem.
	if err := os.Mkdir(filepath.Join(workDir, mutationsSidecarFile), 0o755); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	prevOutput := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	})

	r := sidecarMutationRecorder{kind: "pr"}
	r.RecordExternalRef(context.Background(), providers.ExternalRef{
		Provider: providers.ProviderGitHub, Ref: "acme/app#7", Operation: "open",
	})

	if !strings.Contains(logs.String(), "mutation sidecar") || !strings.Contains(logs.String(), mutationsSidecarFile) {
		t.Fatalf("log output = %q, want a mutation-sidecar failure line naming %s", logs.String(), mutationsSidecarFile)
	}
}
