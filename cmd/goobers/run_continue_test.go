package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

func TestContinuationRepositoryIdentityRequiresEveryComponent(t *testing.T) {
	configured := providers.RepositoryRef{
		Provider: providers.ProviderGitHub, Owner: "Acme", Name: "Web",
	}
	source := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	if !sameContinuationRepository(source, configured) {
		t.Fatal("matching repository identity was rejected")
	}
	source.Name = "other"
	if sameContinuationRepository(source, configured) {
		t.Fatal("repository name mismatch was accepted")
	}
}

func TestContinuationBranchNamespaceNormalizesConfiguredPrefix(t *testing.T) {
	if got := providers.NormalizeBranchNamespace("goobers/implementation"); got != "goobers/implementation/" {
		t.Fatalf("normalized namespace = %q", got)
	}
}

func TestRunRunContinueRejectsLegacySourceWithoutWorkflowPin(t *testing.T) {
	root := t.TempDir()
	runsDir := instance.NewLayout(root).RunsDir()
	sourceID := "0af7651916cd43dd8448eb211c80319c"
	source, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: sourceID, Workflow: "wf", Gaggle: "g",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Append(journal.Event{
		Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
	}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(root, "input.txt")
	if err := os.WriteFile(inputPath, []byte("operator input"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := runRunContinue([]string{
		"--from", sourceID, "--terminal-seq", "2", "--target", "implement",
		"--operator", "operator@example.test", "--integrity", "maintainer",
		"--input", "issue=" + inputPath, root,
	}, &stdout, &stderr)
	if exitCode == 0 {
		t.Fatalf("legacy source was admitted; stdout = %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "resolve continuation source workflow") {
		t.Fatalf("stderr = %s, want source workflow resolution error", stderr.String())
	}
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("runs directory entries = %d, want only source run", len(entries))
	}
}

func TestRunRunContinueResolvesEachHistoricalDigestBeforeCreatingWork(t *testing.T) {
	root := initDeterministicDemo(t)
	runsDir := instance.NewLayout(root).RunsDir()
	candidate, err := currentWorkflowMachine(root, journal.RunIdentity{
		Workflow: "default-implement", WorkflowVersion: 1, Gaggle: "example",
	})
	if err != nil {
		t.Fatalf("currentWorkflowMachine: %v", err)
	}
	for index, command := range []string{"historical-one", "historical-two"} {
		machine, err := workflow.Compile(workflow.Definition{
			Name: "default-implement", Version: 1,
			Spec: apiv1.WorkflowSpec{
				Gaggle: "example", Start: "local-ci",
				Tasks: []apiv1.Task{{
					Name: "local-ci", Type: apiv1.TaskDeterministic,
					Goal: "historical command", Run: &apiv1.DeterministicRun{Command: []string{command}},
				}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		sourceID := []string{"historical-source-a", "historical-source-b"}[index]
		definition, err := json.Marshal(machine.Def)
		if err != nil {
			t.Fatal(err)
		}
		source, err := journal.Create(runsDir, journal.RunIdentity{
			RunID: sourceID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
			WorkflowDigest: machine.Digest(), Gaggle: "example",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
		}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
			journal.WithInputIntegrity(map[string]apiv1.Integrity{
				journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
			}))
		if err != nil {
			t.Fatal(err)
		}
		if err := source.Append(journal.Event{
			Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
		}); err != nil {
			t.Fatal(err)
		}
		if err := source.Close(); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		exitCode := runRunContinue([]string{
			"--from", sourceID, "--terminal-seq", "2", "--target", "local-ci",
			"--operator", "operator@example.test", root,
		}, &stdout, &stderr)
		if exitCode == 0 {
			t.Fatalf("historical source %q was admitted", sourceID)
		}
		if !strings.Contains(stderr.String(), machine.Digest()) {
			t.Fatalf("stderr = %s, want historical digest %s", stderr.String(), machine.Digest())
		}
		if !strings.Contains(stderr.String(), candidate.Digest()) {
			t.Fatalf("stderr = %s, want candidate digest %s", stderr.String(), candidate.Digest())
		}
	}
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("runs directory entries = %d, want only two historical sources", len(entries))
	}
}

func TestRunRunContinueReclaimsRetainedSourceClaims(t *testing.T) {
	root := initDeterministicDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.setBranchTip("goobers/continuation-reclaim", "abc1234")
	server.addIssue(7, "Continuation claim", "goobers", "goobers:ready")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_READ", "continuation-reclaim")
	runsDir := instance.NewLayout(root).RunsDir()
	machine, err := currentWorkflowMachine(root, journal.RunIdentity{
		Workflow: "default-implement", WorkflowVersion: 1, Gaggle: "example",
	})
	if err != nil {
		t.Fatalf("currentWorkflowMachine: %v", err)
	}
	definition, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	const sourceID = "historical-source-reclaim"
	source, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: sourceID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "example",
		WorkspaceBranch:    "goobers/continuation-reclaim",
		WorkspaceBranchSHA: "abc1234",
		WorkspaceRepository: &apiv1.RepoRef{
			Provider: apiv1.ProviderGitHub, Owner: "your-org", Name: "your-repo",
		},
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
		journal.WithInputIntegrity(map[string]apiv1.Integrity{
			journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
		}))
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(instance.NewLayout(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "7"}
	if ok, _, err := ledger.ClaimScoped(key, sourceID, machine.Def.Name, time.Hour); err != nil || !ok {
		t.Fatalf("seed source claim: ok=%v err=%v", ok, err)
	}
	if err := ledger.ReleaseScoped(key, sourceID); err != nil {
		t.Fatalf("release source claim: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := runRunContinue([]string{
		"--from", sourceID, "--terminal-seq", "2", "--target", "local-ci",
		"--operator", "operator@example.test", root,
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("run continue failed: code=%d stderr=%s", exitCode, stderr.String())
	}
	continuationID := strings.TrimSpace(stdout.String())
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(instance.NewLayout(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	claims := reopened.ForRunAll(continuationID)
	if len(claims) != 1 || claims[0].ExternalID != "7" {
		t.Fatalf("continuation claims = %+v", claims)
	}
}

func TestRunRunContinueRefusesWhenRetainedSourceClaimsWereReclaimedElsewhere(t *testing.T) {
	root := initDeterministicDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.setBranchTip("goobers/continuation-conflict", "def5678")
	server.addIssue(7, "Continuation claim", "goobers", "goobers:ready")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_READ", "continuation-conflict")
	runsDir := instance.NewLayout(root).RunsDir()
	machine, err := currentWorkflowMachine(root, journal.RunIdentity{
		Workflow: "default-implement", WorkflowVersion: 1, Gaggle: "example",
	})
	if err != nil {
		t.Fatalf("currentWorkflowMachine: %v", err)
	}
	definition, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	const sourceID = "historical-source-conflict"
	source, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: sourceID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "example",
		WorkspaceBranch:    "goobers/continuation-conflict",
		WorkspaceBranchSHA: "def5678",
		WorkspaceRepository: &apiv1.RepoRef{
			Provider: apiv1.ProviderGitHub, Owner: "your-org", Name: "your-repo",
		},
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
		journal.WithInputIntegrity(map[string]apiv1.Integrity{
			journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
		}))
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(instance.NewLayout(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "7"}
	if ok, _, err := ledger.ClaimScoped(key, sourceID, machine.Def.Name, time.Hour); err != nil || !ok {
		t.Fatalf("seed source claim: ok=%v err=%v", ok, err)
	}
	if err := ledger.ReleaseScoped(key, sourceID); err != nil {
		t.Fatalf("release source claim: %v", err)
	}
	if ok, _, err := ledger.ClaimScoped(key, "other-run", machine.Def.Name, time.Hour); err != nil || !ok {
		t.Fatalf("seed conflicting claim: ok=%v err=%v", ok, err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := runRunContinue([]string{
		"--from", sourceID, "--terminal-seq", "2", "--target", "local-ci",
		"--operator", "operator@example.test", root,
	}, &stdout, &stderr)
	if exitCode == 0 {
		t.Fatalf("run continue unexpectedly succeeded: stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), `source claims are now held by run "other-run"`) {
		t.Fatalf("stderr = %s", stderr.String())
	}
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != sourceID {
		t.Fatalf("runs directory entries = %+v, want only %s", entries, sourceID)
	}
}

func TestRunRunContinueRejectsClosedRetainedClaimBeforeCreatingContinuation(t *testing.T) {
	root := initDeterministicDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.setBranchTip("goobers/continuation-closed", "9876abc")
	server.addIssue(7, "Closed item", "goobers", "goobers:ready")
	server.closeIssue(7)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_READ", "continuation-closed")
	runsDir := instance.NewLayout(root).RunsDir()
	machine, err := currentWorkflowMachine(root, journal.RunIdentity{
		Workflow: "default-implement", WorkflowVersion: 1, Gaggle: "example",
	})
	if err != nil {
		t.Fatalf("currentWorkflowMachine: %v", err)
	}
	definition, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	const sourceID = "historical-source-closed"
	source, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: sourceID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "example",
		WorkspaceBranch:    "goobers/continuation-closed",
		WorkspaceBranchSHA: "9876abc",
		WorkspaceRepository: &apiv1.RepoRef{
			Provider: apiv1.ProviderGitHub, Owner: "your-org", Name: "your-repo",
		},
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
		journal.WithInputIntegrity(map[string]apiv1.Integrity{
			journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
		}))
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(instance.NewLayout(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "7"}
	if ok, _, err := ledger.ClaimScoped(key, sourceID, machine.Def.Name, time.Hour); err != nil || !ok {
		t.Fatalf("seed source claim: ok=%v err=%v", ok, err)
	}
	if err := ledger.ReleaseScoped(key, sourceID); err != nil {
		t.Fatalf("release source claim: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := runRunContinue([]string{
		"--from", sourceID, "--terminal-seq", "2", "--target", "local-ci",
		"--operator", "operator@example.test", root,
	}, &stdout, &stderr)
	if exitCode == 0 {
		t.Fatalf("run continue unexpectedly succeeded: stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), `source claim "7" is no longer open (state "closed")`) {
		t.Fatalf("stderr = %s", stderr.String())
	}
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != sourceID {
		t.Fatalf("runs directory entries = %+v, want only %s", entries, sourceID)
	}
}
