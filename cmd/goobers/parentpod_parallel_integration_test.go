//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/client"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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

type parallelParentWorker struct {
	client.Client
	call   func(context.Context, engine.ChildDispatchInput) (engine.ChildDispatchResult, error)
	starts atomic.Int32
}

func (w *parallelParentWorker) ExecuteWorkflow(ctx context.Context, _ client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
	w.starts.Add(1)
	in := args[0].(engine.ChildDispatchInput)
	out, err := w.call(ctx, in)
	out.BindingDigest = in.BindingDigest()
	return factoryWorkerRun{result: out}, err
}
func (*parallelParentWorker) SignalWorkflow(context.Context, string, string, string, interface{}) error {
	return nil
}

type parallelParentHandoff struct {
	park              bool
	accepted          chan apiv1.InvocationEnvelope
	suspended         chan struct{}
	suspends, resumes atomic.Int32
}

func (h *parallelParentHandoff) Await(ctx context.Context, env apiv1.InvocationEnvelope) (runner.ChildHandoffRequest, error) {
	if h.park && strings.HasSuffix(env.TaskID, ":a") && env.Attempt == 1 {
		select {
		case accepted := <-h.accepted:
			return runner.ChildHandoffRequest{Gaggle: env.Gaggle, ParentRunID: env.RunID, RequestID: journal.Digest([]byte("parallel-wait")), Action: "wait", ChildRunID: "child-run", AcceptanceID: "trigger-child-run", InvocationKey: "child", SourceDigest: journal.Digest([]byte("child-source")), Origin: *accepted.ChildWorkflowOrigin}, nil
		case <-ctx.Done():
			return runner.ChildHandoffRequest{}, ctx.Err()
		}
	}
	<-ctx.Done()
	return runner.ChildHandoffRequest{}, ctx.Err()
}
func (*parallelParentHandoff) Yield(_ context.Context, _ runner.ChildHandoffRequest, custody runner.ChildWorkspaceCustody) error {
	if _, err := os.Stat(filepath.Join(custody.Path, "a.txt")); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(custody.Path, "b.txt")); !os.IsNotExist(err) {
		return errors.New("sibling tree leaked into child owner")
	}
	// Represents the verified host disposition callback, after writer join.
	return os.WriteFile(filepath.Join(custody.Path, "child-result.txt"), []byte("only branch a"), 0600)
}
func (h *parallelParentHandoff) Wait(ctx context.Context, _ runner.ChildHandoffRequest) (runner.ChildHandoffCompletion, error) {
	select {
	case <-h.suspended:
		return runner.ChildHandoffCompletion{State: "completed", ResultRef: "verified-result", Summary: "branch child complete"}, nil
	case <-ctx.Done():
		return runner.ChildHandoffCompletion{}, ctx.Err()
	}
}
func (h *parallelParentHandoff) SuspendChildParent(context.Context, string) (runner.ChildParentSuspension, error) {
	if h.suspends.Add(1) == 1 {
		close(h.suspended)
	}
	return h, nil
}
func (h *parallelParentHandoff) Resume(context.Context) error { h.resumes.Add(1); return nil }

func parallelParentSource(width int) func(string) string {
	return func(source string) string {
		source = strings.Replace(source, "      goal:", "      next: fan\n      goal:", 1)
		for _, name := range []string{"a", "b", "join"} {
			repoFrom, next := "[plan]", "'@join'"
			if name == "join" {
				repoFrom, next = "[plan, a, b]", "''"
			}
			source += fmt.Sprintf(`    - name: %s
      type: agentic
      goober: coder
      goal: Isolated contribution
      retry: {maxAttempts: 1}
      next: %s
      workspace: repo
      repoFrom: %s
      runsOn: {os: linux, capabilities: [isolated-parent]}
      capabilities: [agent:model]
      childWorkflows:
        allowedGoobers: [coder]
        allowedCapabilities: [agent:model]
`, name, next, repoFrom)
		}
		return source + fmt.Sprintf(`  parallels:
    - name: fan
      join: join
      maxConcurrentBranches: %d
      failurePolicy: continue_on_error
      branches:
        - {name: a, start: a}
        - {name: b, start: b}
`, width)
	}
}

// Real Runner.Start, compiler, managed Git forks, installed parent factory,
// bounded transport and returned-tree import. Only the external worker and
// child queue are fixtures; no assertion treats their response as writer proof.
func TestIntegrationParentParallelForksAndJoinUseDeclaredContributions(t *testing.T) {
	for _, parked := range []bool{false, true} {
		t.Run(fmt.Sprint("parked=", parked), func(t *testing.T) { runParentParallelFixture(t, parked) })
	}
}

func runParentParallelFixture(t *testing.T, parked bool) {
	t.Helper()
	testdep.Require(t, "git")
	width := 2
	if parked {
		width = 1
	}
	f := containedParentFixture(t, parallelParentSource(width))
	_, machine, err := childStageCatalog(f.cfg, f.applied, childworkflow.ParentSelection{Gaggle: f.parent.Gaggle, Workflow: f.parent.Workflow, Stage: "plan"}, childworkflow.BackendRunner)
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	recoveryCLIGit(t, repo, "init", "--initial-branch=main")
	if err = os.WriteFile(filepath.Join(repo, "source.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, repo, "add", ".")
	recoveryCLIGit(t, repo, "commit", "-m", "base")
	head, index := recoveryCLIGit(t, repo, "rev-parse", "HEAD"), recoveryCLIGit(t, repo, "write-tree")
	layout, err := instance.EffectiveWorkcopiesLayout(f.layout.ForGaggle(f.parent.Gaggle), f.cfg, &f.applied.Gaggles[0])
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
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
	handoff := &parallelParentHandoff{park: parked, accepted: make(chan apiv1.InvocationEnvelope, 1), suspended: make(chan struct{})}
	worker := &parallelParentWorker{}
	var branches atomic.Int32
	both := make(chan struct{})
	var once sync.Once
	worker.call = func(ctx context.Context, in engine.ChildDispatchInput) (engine.ChildDispatchResult, error) {
		a := in.Attempt
		dir, err := f.layout.FindRunDir(a.RunID)
		if err != nil {
			return engine.ChildDispatchResult{}, err
		}
		reader, err := journal.OpenReadOnly(dir)
		if err != nil {
			return engine.ChildDispatchResult{}, err
		}
		id, err := reader.Identity()
		if err != nil {
			return engine.ChildDispatchResult{}, err
		}
		store := childpod.ParentAttemptBlobs{Store: childpod.ParentBlobs{RunDir: dir, Identity: id}, ContractDigest: a.ChildExecutionDigest}
		data, err := store.Get(ctx, a.ChildExecutionDigest)
		if err != nil {
			return engine.ChildDispatchResult{}, err
		}
		contract, err := childpod.DecodeContract(data, a.ChildExecutionDigest)
		if err != nil {
			return engine.ChildDispatchResult{}, err
		}
		pod := t.TempDir()
		if err = childpod.Materialize(ctx, pod, *contract.Workspace); err != nil {
			return engine.ChildDispatchResult{}, err
		}
		if err = parallelParentPodFiles(ctx, pod, a, reader, parked); err != nil {
			return engine.ChildDispatchResult{}, err
		}
		if !parked && (a.Stage == "a" || a.Stage == "b") {
			if branches.Add(1) == 2 {
				once.Do(func() { close(both) })
			}
			select {
			case <-both:
			case <-ctx.Done():
				return engine.ChildDispatchResult{}, ctx.Err()
			}
		}
		if parked && a.Stage == "a" && a.Number == 1 {
			handoff.accepted <- *a.Envelope
			<-ctx.Done()
		}
		custody, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		carrier, _, err := childpod.CaptureCarrier(custody, pod, contract.Workspace.Snapshot.Record.RepositoryKey, id.RunID, contract.StartedAt, contract.Workspace.Snapshot.Policy)
		if err != nil {
			return engine.ChildDispatchResult{}, err
		}
		data, _ = json.Marshal(childpod.Output{Version: 1, ContractDigest: a.ChildExecutionDigest, Workspace: &carrier})
		digest := journal.Digest(data)
		if err = store.Put(custody, digest, data); err != nil {
			return engine.ChildDispatchResult{}, err
		}
		data, _ = json.Marshal(dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, ChildWorkspaceDigest: digest})
		if err = plane.Put(custody, a.RunID, a.Stage, a.PodAttempt, data); err != nil {
			return engine.ChildDispatchResult{}, err
		}
		return engine.ChildDispatchResult{Report: dispatcher.Report{ChildCreateAttempted: true, ChildPodUID: fmt.Sprint("pod-", a.PodAttempt), WorkspaceWritersStopped: true, SurrenderConfirmed: true}}, nil
	}
	service.installChildPodFactories(worker, plane)
	project := f.applied.Gaggles[0].Spec.Project
	config := runner.Config{Worktrees: manager, RunsDir: f.layout.ForGaggle(f.parent.Gaggle).RunsDir(), ScratchDir: t.TempDir(), ConfigGeneration: f.parent.ConfigGeneration, InstanceID: f.parent.InstanceID, RepoCloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil }, ChildHandoff: handoff, ChildParentCapacity: handoff, SelfExecutionDenied: true, NewAgentic: func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) {
		return &parentRouteFake{}, nil
	}}
	config = withContainedParentExecutor(config, f.layout.Root, f.cfg, f.applied)
	driver, err := runner.New(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	runID := strings.Repeat("e", 32)
	result, err := driver.Start(ctx, runner.StartInput{RunID: runID, Gaggle: f.parent.Gaggle, Machine: machine, GooberDigest: f.parent.GooberDigest, RepoRef: project})
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatal(result, err)
	}
	if parked && (handoff.suspends.Load() != 1 || handoff.resumes.Load() != 1 || worker.starts.Load() != 5) {
		t.Fatal("parallel wait accounting", handoff.suspends.Load(), handoff.resumes.Load(), worker.starts.Load())
	}
	dir, err := f.layout.FindRunDir(runID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertParallelParentArchives(t, reader, manager, repo, parked)
	if recoveryCLIGit(t, repo, "rev-parse", "HEAD") != head || recoveryCLIGit(t, repo, "write-tree") != index {
		t.Fatal("parent base HEAD/index mutated")
	}
	if data, err := os.ReadFile(filepath.Join(repo, "source.txt")); err != nil || string(data) != "base" {
		t.Fatal("source checkout mutated")
	}
}

func parallelParentPodFiles(ctx context.Context, pod string, a dispatcher.Attempt, reader *journal.Reader, parked bool) error {
	if a.Stage == "plan" {
		return os.WriteFile(filepath.Join(pod, "seed.txt"), []byte("seed dirty input"), 0600)
	}
	if data, err := os.ReadFile(filepath.Join(pod, "seed.txt")); err != nil || string(data) != "seed dirty input" {
		return errors.New("branch lost seed contribution")
	}
	if a.Stage == "join" {
		events, err := reader.Events()
		if err != nil {
			return err
		}
		selected := ""
		for _, ev := range events {
			if ev.Type == journal.EventStageFinished && (ev.Stage == "a" || ev.Stage == "b") {
				selected = ev.Stage
			}
		}
		data, err := os.ReadFile(filepath.Join(pod, "source.txt"))
		if err != nil || string(data) != selected {
			return fmt.Errorf("join did not select latest declared producer: %s / %s", data, selected)
		}
		other := "a"
		if selected == "a" {
			other = "b"
		}
		if _, err = os.Stat(filepath.Join(pod, other+".txt")); !os.IsNotExist(err) {
			return errors.New("join silently merged independent branch")
		}
		return nil
	}
	other := "a"
	if a.Stage == "a" {
		other = "b"
	}
	if _, err := os.Stat(filepath.Join(pod, other+".txt")); !os.IsNotExist(err) {
		return errors.New("branch inherited sibling work")
	}
	if parked && a.Stage == "a" && a.Number > 1 {
		if _, err := os.Stat(filepath.Join(pod, "child-result.txt")); err != nil {
			return errors.New("same-occurrence continuation lost child disposition")
		}
	}
	if a.Stage == "b" {
		if _, err := os.Stat(filepath.Join(pod, "child-result.txt")); !os.IsNotExist(err) {
			return errors.New("child disposition escaped owning branch")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pod, "source.txt"), []byte(a.Stage), 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(pod, a.Stage+".txt"), []byte(a.Stage), 0600)
}

func assertParallelParentArchives(t *testing.T, reader *journal.Reader, manager *worktree.Manager, repo string, parked bool) {
	t.Helper()
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	workspaces := map[string]bool{}
	archives := map[string]bool{}
	var origins []*apiv1.ChildWorkflowOrigin
	for _, event := range events {
		if event.Runner["kind"] == runner.ContainedParentWorkspaceKind {
			var custody runner.ContainedParentWorkspaceCustody
			data, _ := json.Marshal(event.Runner["custody"])
			if err := json.Unmarshal(data, &custody); err != nil {
				t.Fatal(err)
			}
			workspaces[custody.Workspace.WorkspaceID] = true
			if _, err := manager.AdoptHeldStage(t.Context(), repo, custody.Workspace); err == nil {
				t.Fatal("completed fork was not retired")
			}
			if event.Stage == "a" {
				origins = append(origins, custody.Origin)
			}
		}
		if event.Runner["kind"] == runner.ParentContributionKind {
			var receipt struct{ Output journal.Ref }
			raw, _ := json.Marshal(event.Runner["contribution"])
			if err := json.Unmarshal(raw, &receipt); err != nil {
				t.Fatal(err)
			}
			data, err := reader.ArtifactBytesBounded(receipt.Output, 24<<20)
			if err != nil {
				t.Fatal(err)
			}
			var output childpod.Output
			if err := json.Unmarshal(data, &output); err != nil || output.Workspace == nil {
				t.Fatal(err)
			}
			path := t.TempDir()
			if err := childpod.Materialize(t.Context(), path, *output.Workspace); err != nil {
				t.Fatal("unselected contribution no longer readable", err)
			}
			archives[event.Stage] = true
		}
	}
	if len(workspaces) != 3 || len(archives) != 4 {
		t.Fatal("missing independent retained forks", workspaces, archives)
	}
	if pending, err := runner.PendingParentContributions(events); err != nil || pending {
		t.Fatal("fork retirement unresolved", pending, err)
	}
	if parked && (len(origins) != 2 || origins[0].StageOccurrence != origins[1].StageOccurrence || origins[0].AttemptID == origins[1].AttemptID) {
		t.Fatal("wait consumed logical occurrence", origins)
	}
}
