//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
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
	testParentFactoryCustody(t, false, false)
}
func TestIntegrationParentRecoveryRejoinsOriginalWorkerAndPreservesDirtyTree(t *testing.T) {
	testdep.Require(t, "git")
	testParentFactoryCustody(t, true, false)
}
func TestIntegrationParentRecoveryPreservesCommittedStagedAndDirtyState(t *testing.T) {
	testdep.Require(t, "git")
	testParentFactoryCustody(t, true, true)
}
func testParentFactoryCustody(t *testing.T, lost, committed bool) {
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
	source := repo
	baseSHA := recoveryCLIGit(t, repo, "rev-parse", "HEAD")
	layout, err := instance.EffectiveWorkcopiesLayout(f.layout.ForGaggle(env.Gaggle), f.cfg, &f.applied.Gaggles[0])
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	url, err := childRepoCloneURL(env.RepoRef)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.WithRecoveryMirror(t.Context(), url, func(mirror string) error { recoveryCLIGit(t, mirror, "fetch", source, "main"); return nil }); err != nil {
		t.Fatal(err)
	}
	checkout, err := manager.CreateChildFromSnapshot(t.Context(), worktree.ChildOptions{RepoURL: url, RunID: env.RunID + "-parent", OwnerRunID: env.RunID, Gaggle: env.Gaggle, SnapshotSHA: baseSHA})
	if err != nil {
		t.Fatal(err)
	}
	repo = checkout.Path
	env.Workspace = repo
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
	held, err := checkout.HoldForChild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	custody := runner.ContainedParentWorkspaceCustody{Version: 1, Origin: env.ChildWorkflowOrigin, Workspace: held}
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
	service.childDispatch = newDaemonTriggerService()
	plane, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identityReader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := identityReader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	store := childpod.ParentBlobs{RunDir: run.Dir(), Identity: id}
	callCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	worker := &parentWorkerClient{factoryWorkerClient: factoryWorkerClient{lostReply: lost}}
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
		if err = childpod.MaterializeContract(ctx, pod, contract); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(pod, "source.txt")); err != nil || string(got) != "parent dirty\n" {
			t.Fatal("dirty parent missing from isolated fork", string(got), err)
		}
		if recoveryCLIGit(t, pod, "show", "HEAD:source.txt") != "base" || recoveryCLIGit(t, pod, "show", ":source.txt") != "staged" {
			t.Fatal("parent Git states were flattened before invocation")
		}
		if committed {
			recoveryCLIGit(t, pod, "commit", "-m", "commit only previously staged input")
			if err = os.WriteFile(filepath.Join(pod, "source.txt"), []byte("pod staged\n"), 0600); err != nil {
				t.Fatal(err)
			}
			recoveryCLIGit(t, pod, "add", "source.txt")
		}
		if err = os.WriteFile(filepath.Join(pod, "source.txt"), []byte("pod edited before yield\n"), 0600); err != nil {
			t.Fatal(err)
		}
		output, err := childpod.CaptureOutput(ctx, pod, contract, a.ChildExecutionDigest)
		if err != nil {
			t.Fatal(err)
		}
		data, _ = json.Marshal(output)
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
	service.installChildPodFactories(worker, plane, childFactoryJournals(t, service))
	executor := &parentStagePod{service: service, journal: run, identity: id, client: worker, surrenders: plane, goober: env.Goober}
	ctx, proof := invoke.WithWorkspaceQuiescence(callCtx)
	var observed map[string]float64
	ctx = invoke.WithAgentUsageReporter(ctx, func(m map[string]float64) { observed = m })
	out, err := executor.Invoke(ctx, env)
	if lost {
		if err == nil || proof.Verify() == nil {
			t.Fatal("uncertain dispatch released workspace", err)
		}
		if got, err := os.ReadFile(filepath.Join(repo, "source.txt")); err != nil || string(got) != "parent dirty\n" {
			t.Fatal("unknown dispatch changed parent", string(got), err)
		}
		registry := newDaemonRunnerRegistry()
		service.installParentRecovery(registry)
		resolutions := 0
		registry.setGenerationResolver(func(context.Context, journal.RunIdentity) (executionGenerationRuntime, error) {
			resolutions++
			return executionGenerationRuntime{}, nil
		})
		untrack, ok := registry.TrackCompatible(id.RunID, &runner.Runner{})
		if !ok {
			t.Fatal("could not establish live owner")
		}
		if _, err := registry.executionGeneration(t.Context(), id); !errors.Is(err, journal.ErrRecoveryBusy) || worker.gets != 0 || resolutions != 0 {
			t.Fatal("recovery borrowed live runner", err)
		}
		untrack()
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.executionGeneration(t.Context(), id); err != nil {
			t.Fatal("exact worker recovery", err)
		}
		if worker.gets != 1 || resolutions != 1 {
			t.Fatal("original worker was not rejoined", worker.gets, resolutions)
		}
		pending, events, err := childpod.PendingParentScopes(t.Context(), identityReader)
		if err != nil || len(pending) != 0 {
			t.Fatal("recovered worker remains pending", pending, err)
		}
		recovered := 0
		for _, event := range events {
			if event.Runner["kind"] != runner.ContainedParentRecoveredKind {
				continue
			}
			recovered++
			raw, _ := json.Marshal(event.Runner["recovery"])
			var ref journal.Ref
			if err := json.Unmarshal(raw, &ref); err != nil {
				t.Fatal(err)
			}
			data, err := identityReader.ArtifactBytes(ref)
			if err != nil {
				t.Fatal(err)
			}
			var record struct {
				Transcript *apiv1.ArtifactPointer
				Usage      map[string]float64
			}
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			if record.Usage["tokens.input"] != 23 {
				t.Fatal("supervised usage lost", record.Usage)
			}
			out.Transcript = record.Transcript
		}
		if recovered != 1 || out.Transcript == nil {
			t.Fatal("recovery receipt missing", recovered)
		}
		if _, err := registry.executionGeneration(t.Context(), id); err != nil || worker.gets != 1 {
			t.Fatal("settled recovery contacted worker again", err)
		}
	} else if err != nil || out.Transcript == nil || proof.Verify() != nil {
		t.Fatal(out, err, proof.Verify())
	}
	if worker.stops != 1 || worker.starts != 1 || (!lost && observed["tokens.input"] != 23) {
		t.Fatal(worker.stops, worker.starts, observed)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "source.txt")); err != nil || string(got) != "pod edited before yield\n" {
		t.Fatal(string(got), err)
	}
	wantIndex := "staged"
	if committed {
		wantIndex = "pod staged"
		if recoveryCLIGit(t, repo, "rev-parse", "HEAD^") != head || recoveryCLIGit(t, repo, "show", "HEAD:source.txt") != "staged" {
			t.Fatal("parent checkpoint lost real ancestry or committed dirty input")
		}
	} else if recoveryCLIGit(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("uncommitted return moved parent HEAD")
	}
	if recoveryCLIGit(t, repo, "show", ":unrelated.txt") != "keep staged" || recoveryCLIGit(t, repo, "show", ":source.txt") != wantIndex {
		t.Fatal("returned index was flattened into working files")
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reader.ArtifactBytes(journal.Ref{Path: out.Transcript.Path, Digest: out.Transcript.Digest, Size: out.Transcript.Size}); err != nil {
		t.Fatal("partial transcript not adopted", err)
	}
	if committed {
		key := (providers.RepositoryRef{Provider: providers.ProviderKind(env.RepoRef.Provider), URL: env.RepoRef.BaseURL, Owner: env.RepoRef.Owner, Project: env.RepoRef.Project, Name: env.RepoRef.Name}).CanonicalKey()
		verifyLatestParentCleanupArchive(t, reader, repo, key, held)
	}
}
