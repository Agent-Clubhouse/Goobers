//go:build integration

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/client"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

type parentWorkerClient struct {
	factoryWorkerClient
	stops int
}

func (c *parentWorkerClient) SignalWorkflow(context.Context, string, string, string, interface{}) error {
	c.stops++
	return nil
}

func TestIntegrationParentFactoryCancellationReturnsDirtyTreeAndTranscript(t *testing.T) {
	testdep.Require(t, "git")
	f := containedParentFixture(t)
	run, env := configuredChildStage(t, f)
	env.RepoRef = f.applied.Gaggles[0].Spec.Project
	repo := env.Workspace
	recoveryCLIGit(t, repo, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, repo, "add", ".")
	recoveryCLIGit(t, repo, "commit", "-m", "base")
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("staged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, repo, "add", ".")
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("parent dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("keep staged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, repo, "add", "unrelated.txt")
	head := recoveryCLIGit(t, repo, "rev-parse", "HEAD")
	custody := runner.ContainedParentWorkspaceCustody{Version: 1, Origin: env.ChildWorkflowOrigin, Workspace: worktree.StageCustody{WorkspaceID: "fixture-checkout", OwnerRunID: env.RunID, RepositoryDigest: worktree.RepositoryDigest(repo), Branch: "main", StartRef: head}}
	if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "plan", Attempt: int(env.Attempt), Runner: map[string]any{"kind": runner.ContainedParentWorkspaceKind, "custody": custody}}); err != nil {
		t.Fatal(err)
	}
	index := recoveryCLIGit(t, repo, "write-tree")
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	service := newDaemonCredentialService(f.layout, f.cfg, nil, journal.NewRegistryScrubber(), nil).withStageGrants(f.layout.Root, "127.0.0.1:8080", false)
	t.Cleanup(func() { unregisterDaemonStageGrants(f.layout.Root, service) })
	if err = service.enableChildWorkflows(queue, f.applied); err != nil {
		t.Fatal(err)
	}
	plane, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, id, err := parentJournal(run)
	if err != nil {
		t.Fatal(err)
	}
	store := childpod.ParentBlobs{RunDir: run.Dir(), Identity: id}
	callCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	worker := &parentWorkerClient{}
	worker.execute = func(ctx context.Context, in engine.ChildDispatchInput) (engine.ChildDispatchResult, error) {
		if recoveryCLIGit(t, repo, "write-tree") != index || recoveryCLIGit(t, repo, "rev-parse", "HEAD") != head {
			t.Fatal("capturing pod input changed parent index or history")
		}
		a := in.Attempt
		if !a.WorkflowParent || a.Envelope == nil || a.Envelope.Workspace != "" {
			t.Fatal("host workspace leaked into worker attempt", a)
		}
		scoped := childpod.ParentAttemptBlobs{Store: store, ContractDigest: a.ChildExecutionDigest}
		data, err := scoped.Get(ctx, a.ChildExecutionDigest)
		if err != nil {
			t.Fatal(err)
		}
		contract, err := childpod.DecodeContract(data, a.ChildExecutionDigest)
		if err != nil {
			t.Fatal(err)
		}
		if contract.ParentOrigin == nil || *contract.ParentOrigin != *env.ChildWorkflowOrigin || contract.PodAttempt != a.PodAttempt || contract.Identity.Child != nil {
			t.Fatal(contract)
		}
		reader, err := journal.OpenReadOnly(run.Dir())
		if err != nil {
			t.Fatal(err)
		}
		started, err := childPodStarted(reader, a.Stage, a.Number, false)
		if err != nil {
			t.Fatal(err)
		}
		if uint64(a.PodAttempt) != started.Seq || !contract.StartedAt.Equal(started.Time) {
			t.Fatal("physical attempt is not journal bound")
		}
		data, err = scoped.Get(ctx, a.KitDigest)
		if err != nil {
			t.Fatal(err)
		}
		var kit agentickit.Kit
		if err = json.Unmarshal(data, &kit); err != nil {
			t.Fatal(err)
		}
		if kit.Envelope.Workspace != "" || kit.Envelope.ChildWorkflowOrigin == nil || kit.Goobers["coder"].Harness != apiv1.HarnessClaudeCode {
			t.Fatal("incorrect retained parent kit")
		}
		pod := t.TempDir()
		if err = childpod.Materialize(ctx, pod, *contract.Workspace); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(pod, "source.txt")); err != nil || string(got) != "parent dirty\n" {
			t.Fatal("dirty parent missing from isolated fork", string(got), err)
		}
		if err = os.WriteFile(filepath.Join(pod, "source.txt"), []byte("pod edited before yield\n"), 0600); err != nil {
			t.Fatal(err)
		}
		carrier, _, err := childpod.CaptureCarrier(ctx, pod, contract.Workspace.Snapshot.Record.RepositoryKey, id.RunID, contract.StartedAt, contract.Workspace.Snapshot.Policy)
		if err != nil {
			t.Fatal(err)
		}
		data, _ = json.Marshal(childpod.Output{Version: 1, ContractDigest: a.ChildExecutionDigest, Workspace: &carrier})
		digest := journal.Digest(data)
		if err = scoped.Put(ctx, digest, data); err != nil {
			t.Fatal(err)
		}
		transcript := []byte("partial context preserved across child wait")
		ref, err := journal.ArtifactRef(transcript)
		if err != nil {
			t.Fatal(err)
		}
		if err = scoped.Put(ctx, ref.Digest, transcript); err != nil {
			t.Fatal(err)
		}
		out := dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{Code: "yielded", Message: "yielded"}, Transcript: &apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size}}, ChildWorkspaceDigest: digest, ObservedUsageReported: true, ObservedUsage: map[string]float64{"tokens.input": 23}}
		data, _ = json.Marshal(out)
		if err = plane.Put(ctx, a.RunID, a.Stage, a.PodAttempt, data); err != nil {
			t.Fatal(err)
		}
		cancel()
		return engine.ChildDispatchResult{Report: dispatcher.Report{ChildCreateAttempted: true, ChildPodUID: "exact-parent-pod", WorkspaceWritersStopped: true, SurrenderConfirmed: true}}, nil
	}
	service.installChildPodFactories(worker, plane)
	executor, err := service.parentExecutors(run, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	ctx, proof := invoke.WithWorkspaceQuiescence(callCtx)
	var observed map[string]float64
	ctx = invoke.WithAgentUsageReporter(ctx, func(m map[string]float64) { observed = m })
	out, err := executor.Invoke(ctx, env)
	if err != nil || out.Transcript == nil || proof.Verify() != nil {
		t.Fatal(out, err, proof.Verify())
	}
	if worker.stops != 1 || worker.starts != 1 || observed["tokens.input"] != 23 {
		t.Fatal(worker.stops, worker.starts, observed)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "source.txt")); err != nil || string(got) != "pod edited before yield\n" {
		t.Fatal(string(got), err)
	}
	if recoveryCLIGit(t, repo, "rev-parse", "HEAD") != head || recoveryCLIGit(t, repo, "show", ":unrelated.txt") != "keep staged" {
		t.Fatal("parent ancestry or unrelated staging changed")
	}
	if recoveryCLIGit(t, repo, "show", ":source.txt") != "pod edited before yield" {
		t.Fatal("returned changed path was not staged")
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reader.ArtifactBytes(journal.Ref{Path: out.Transcript.Path, Digest: out.Transcript.Digest, Size: out.Transcript.Size}); err != nil {
		t.Fatal("partial transcript not adopted", err)
	}
}

func TestIntegrationParentRunnerStartUsesContainedFactory(t *testing.T) {
	testParentRunnerStart(t, false, false)
}
func TestIntegrationParentRunnerUncertainPodKeepsOriginalWorkspace(t *testing.T) {
	testParentRunnerStart(t, true, false)
}
func TestIntegrationParentReconcilesExactWorkerAndReusesHeldWorkspace(t *testing.T) {
	testParentRunnerStart(t, true, true)
}

type recoverableParentWorker struct {
	factoryWorkerClient
	recovered engine.ChildDispatchResult
}

func (c *recoverableParentWorker) GetWorkflow(context.Context, string, string) client.WorkflowRun {
	return factoryWorkerRun{result: c.recovered}
}
func (c *recoverableParentWorker) SignalWorkflow(context.Context, string, string, string, interface{}) error {
	return nil
}

func testParentRunnerStart(t *testing.T, uncertain, recoverWorker bool, change ...func(string) string) {
	testdep.Require(t, "git")
	f := containedParentFixture(t, change...)
	_, machine, err := childStageCatalog(f.cfg, f.applied, childworkflow.ParentSelection{Gaggle: f.parent.Gaggle, Workflow: f.parent.Workflow, Stage: "plan"}, childworkflow.BackendRunner)
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	recoveryCLIGit(t, repo, "init", "--initial-branch=main")
	if err = os.WriteFile(filepath.Join(repo, "source.txt"), []byte("source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, repo, "add", ".")
	recoveryCLIGit(t, repo, "commit", "-m", "base")
	scopedLayout, err := instance.EffectiveWorkcopiesLayout(f.layout.ForGaggle(f.parent.Gaggle), f.cfg, &f.applied.Gaggles[0])
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(scopedLayout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	service := newDaemonCredentialService(f.layout, f.cfg, nil, journal.NewRegistryScrubber(), nil).withStageGrants(f.layout.Root, "127.0.0.1:8080", false)
	t.Cleanup(func() { unregisterDaemonStageGrants(f.layout.Root, service) })
	if err = service.enableChildWorkflows(queue, f.applied); err != nil {
		t.Fatal(err)
	}
	service.childDispatch = newDaemonTriggerService()
	plane, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker := &recoverableParentWorker{}
	worker.execute = func(ctx context.Context, in engine.ChildDispatchInput) (engine.ChildDispatchResult, error) {
		a := in.Attempt
		dir, err := f.layout.FindRunDir(a.RunID)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := journal.OpenReadOnly(dir)
		if err != nil {
			t.Fatal(err)
		}
		id, err := reader.Identity()
		if err != nil {
			t.Fatal(err)
		}
		scoped := childpod.ParentAttemptBlobs{Store: childpod.ParentBlobs{RunDir: dir, Identity: id}, ContractDigest: a.ChildExecutionDigest}
		data, err := scoped.Get(ctx, a.ChildExecutionDigest)
		if err != nil {
			t.Fatal(err)
		}
		contract, err := childpod.DecodeContract(data, a.ChildExecutionDigest)
		if err != nil {
			t.Fatal(err)
		}
		if !a.WorkflowParent || contract.ParentOrigin == nil || a.Envelope.Workspace != "" {
			t.Fatal("actual runner escaped contained parent route")
		}
		if recoverWorker || len(change) > 0 {
			pod := t.TempDir()
			if err = childpod.Materialize(ctx, pod, *contract.Workspace); err != nil {
				t.Fatal(err)
			}
			if worker.starts > 1 {
				data, err := os.ReadFile(filepath.Join(pod, "source.txt"))
				if err != nil || string(data) != "recovered edits\n" {
					t.Fatal("recovered tree was reset", string(data), err)
				}
				found := false
				for _, pointer := range a.Envelope.ContextPointers {
					found = found || pointer.Name == "recovered-parent-transcript"
				}
				if !found && len(change) == 0 {
					t.Fatal("recovery transcript not given to continuation")
				}
			}
			if err = os.WriteFile(filepath.Join(pod, "source.txt"), []byte("recovered edits\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if len(change) > 0 {
				if worker.starts > 1 {
					if data, err := os.ReadFile(filepath.Join(pod, "new.txt")); err != nil || string(data) != "untracked contribution" {
						t.Fatal("new contribution was lost", err)
					}
				}
				if err := os.WriteFile(filepath.Join(pod, "new.txt"), []byte("untracked contribution"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			carrier, _, err := childpod.CaptureCarrier(ctx, pod, contract.Workspace.Snapshot.Record.RepositoryKey, id.RunID, contract.StartedAt, contract.Workspace.Snapshot.Policy)
			if err != nil {
				t.Fatal(err)
			}
			contract.Workspace = &carrier
		}
		data, _ = json.Marshal(childpod.Output{Version: 1, ContractDigest: a.ChildExecutionDigest, Workspace: contract.Workspace})
		digest := journal.Digest(data)
		if err = scoped.Put(ctx, digest, data); err != nil {
			t.Fatal(err)
		}
		result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}
		if recoverWorker {
			transcript := []byte("interrupted parent context")
			ref, err := journal.ArtifactRef(transcript)
			if err != nil {
				t.Fatal(err)
			}
			if err = scoped.Put(ctx, ref.Digest, transcript); err != nil {
				t.Fatal(err)
			}
			result.Transcript = &apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size}
		}
		data, _ = json.Marshal(dispatcher.SurrenderedResult{Result: result, ChildWorkspaceDigest: digest})
		if err = plane.Put(ctx, a.RunID, a.Stage, a.PodAttempt, data); err != nil {
			t.Fatal(err)
		}
		worker.recovered = engine.ChildDispatchResult{BindingDigest: in.BindingDigest(), Report: dispatcher.Report{ChildCreateAttempted: true, ChildPodUID: "exact-parent", WorkspaceWritersStopped: true, SurrenderConfirmed: true}}
		if uncertain && worker.starts == 1 {
			return engine.ChildDispatchResult{Report: dispatcher.Report{ChildCreateAttempted: true}}, nil
		}
		return worker.recovered, nil
	}
	service.installChildPodFactories(worker, plane)
	project := f.applied.Gaggles[0].Spec.Project
	clone := func(apiv1.RepoRef) (string, error) { return repo, nil }
	handoff := &daemonChildHandoff{layout: f.layout.ForGaggle(f.parent.Gaggle), worktrees: manager, repoCloneURL: clone, project: project}
	base := &parentRouteFake{}
	config := runner.Config{Worktrees: manager, RunsDir: f.layout.ForGaggle(f.parent.Gaggle).RunsDir(), ScratchDir: t.TempDir(), ConfigGeneration: f.parent.ConfigGeneration, InstanceID: f.parent.InstanceID, RepoCloneURL: clone, ChildHandoff: handoff, ChildParentCapacity: handoff, SelfExecutionDenied: true, NewAgentic: func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) { return base, nil }}
	config = withContainedParentExecutor(config, f.layout.Root, f.cfg, f.applied)
	driver, err := runner.New(config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := driver.Start(t.Context(), runner.StartInput{RunID: strings.Repeat("d", 32), Gaggle: f.parent.Gaggle, Machine: machine, GooberDigest: f.parent.GooberDigest, RepoRef: project})
	if uncertain {
		if err == nil || worker.starts != 1 || base.calls != 0 {
			t.Fatal("unknown pod allowed another writer", result, err, worker.starts, base.calls)
		}
		assertParentWorkspaceRetained(t, f, manager, repo)
		if !recoverWorker {
			return
		}
		previous := repoCloneURL
		repoCloneURL = clone
		t.Cleanup(func() { repoCloneURL = previous })
		dir, err := f.layout.FindRunDir(strings.Repeat("d", 32))
		if err != nil {
			t.Fatal(err)
		}
		reader, err := journal.OpenReadOnly(dir)
		if err != nil {
			t.Fatal(err)
		}
		id, err := reader.Identity()
		if err != nil {
			t.Fatal(err)
		}
		registry := newDaemonRunnerRegistry()
		service.installParentRecovery(registry)
		registry.setGenerationResolver(func(context.Context, journal.RunIdentity) (executionGenerationRuntime, error) {
			return executionGenerationRuntime{runner: driver, machine: machine}, nil
		})
		if _, err = registry.executionGeneration(t.Context(), id); err != nil {
			t.Fatal("exact parent recovery failed", err)
		}
		if worker.starts != 1 {
			t.Fatal("recovery launched another pod")
		}
		if err = verifyParentPodCustody(reader); err != nil {
			t.Fatal(err)
		}
		events, err := reader.Events()
		if err != nil {
			t.Fatal(err)
		}
		var terminal uint64
		for _, event := range events {
			if event.Type == journal.EventRunFinished {
				terminal = event.Seq
			}
		}
		resumed, err := driver.ResumeFromTerminal(t.Context(), runner.ResumeFromTerminalInput{RunID: id.RunID, Machine: machine, GooberDigest: id.GooberDigest, RepoRef: project, Target: "plan", Actor: "operator", Action: "retry", Rationale: "continue recovered work", ExpectedTerminalSeq: terminal})
		if err != nil || resumed.Phase != journal.PhaseCompleted || worker.starts != 2 {
			t.Fatal("recovered parent did not continue", resumed, err, worker.starts)
		}
		return
	}
	if err != nil || result.Phase != journal.PhaseCompleted || worker.starts != len(machine.Def.Spec.Tasks) || base.calls != 0 {
		t.Fatal("actual parent runner failed", result, err, worker.starts, base.calls)
	}
	dir, err := f.layout.FindRunDir(strings.Repeat("d", 32))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyParentPodCustody(reader); err != nil {
		t.Fatal(err)
	}
	if len(change) > 0 {
		assertRetiredParentContribution(t, reader, manager, repo)
	}
}

func assertParentWorkspaceRetained(t *testing.T, f pinnedChildFixture, manager *worktree.Manager, repo string) {
	t.Helper()
	dir, err := f.layout.FindRunDir(strings.Repeat("d", 32))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if verifyParentPodCustody(reader) == nil {
		t.Fatal("unknown parent was acknowledged")
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Runner["kind"] != runner.ContainedParentWorkspaceKind {
			continue
		}
		data, err := json.Marshal(event.Runner["custody"])
		if err != nil {
			t.Fatal(err)
		}
		var receipt runner.ContainedParentWorkspaceCustody
		if err = json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		held, err := manager.AdoptHeldStage(t.Context(), repo, receipt.Workspace)
		if err != nil {
			t.Fatal("uncertain parent worktree was removed", err)
		}
		if _, err = os.Stat(filepath.Join(held.Path, "source.txt")); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("parent workspace has no durable custody receipt")
}

func TestIntegrationParentContributionFeedsDeclaredNextStage(t *testing.T) {
	testParentRunnerStart(t, false, false, func(source string) string {
		return strings.Replace(source, "      goal:", "      next: verify\n      goal:", 1) + `    - name: verify
      type: agentic
      goober: coder
      goal: Verify returned contribution
      workspace: repo
      repoFrom: plan
      runsOn: {os: linux, capabilities: [isolated-parent]}
      capabilities: [agent:model]
      childWorkflows:
        allowedGoobers: [coder]
        allowedCapabilities: [agent:model]
`
	})
}

func assertRetiredParentContribution(t *testing.T, reader *journal.Reader, manager *worktree.Manager, repo string) {
	t.Helper()
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var workspaceID string
	var occurrences []string
	var final journal.Ref
	for _, event := range events {
		if event.Runner["kind"] == runner.ContainedParentWorkspaceKind {
			data, _ := json.Marshal(event.Runner["custody"])
			var custody runner.ContainedParentWorkspaceCustody
			if err := json.Unmarshal(data, &custody); err != nil {
				t.Fatal(err)
			}
			if workspaceID != "" && workspaceID != custody.Workspace.WorkspaceID {
				t.Fatal("declared handoff switched checkout")
			}
			workspaceID = custody.Workspace.WorkspaceID
			occurrences = append(occurrences, custody.Origin.StageOccurrence)
			if _, err := manager.AdoptHeldStage(t.Context(), repo, custody.Workspace); err == nil {
				t.Fatal("completed contribution checkout retained")
			}
		}
		if event.Runner["kind"] == runner.ParentContributionRetiredKind {
			data, _ := json.Marshal(event.Runner["contribution"])
			var receipt struct {
				Output journal.Ref `json:"output"`
			}
			if err := json.Unmarshal(data, &receipt); err != nil {
				t.Fatal(err)
			}
			final = receipt.Output
			if event.Stage != "verify" {
				t.Fatal("retired intermediate contribution")
			}
		}
	}
	if len(occurrences) != 2 || occurrences[0] == occurrences[1] {
		t.Fatal("stage histories not distinct", occurrences)
	}
	data, err := reader.ArtifactBytesBounded(final, 24<<20)
	if err != nil {
		t.Fatal("final source context no longer readable", err)
	}
	var out childpod.Output
	if err := json.Unmarshal(data, &out); err != nil || out.Workspace == nil {
		t.Fatal(err)
	}
	restored := t.TempDir()
	if err := childpod.Materialize(t.Context(), restored, *out.Workspace); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"source.txt": "recovered edits\n", "new.txt": "untracked contribution"} {
		if got, err := os.ReadFile(filepath.Join(restored, path)); err != nil || string(got) != want {
			t.Fatal("archived contribution differs", path, string(got), err)
		}
	}
}
