//go:build integration && (darwin || linux)

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
	"github.com/goobers/goobers/test/testsupport/testdep"
	"sigs.k8s.io/yaml"
)

func trustedLocalJourneyFixture(t *testing.T) pinnedChildFixture {
	t.Helper()
	return newPinnedChildFixture(t, func(root string) {
		parent := strings.Replace(childValidationParent, "      goal:", "      workspace: repo\n      goal:", 1)
		parent = strings.Replace(parent, "allowPRPublication: true", "allowPRPublication: false", 1)
		writeFileContent(t, filepath.Join(root, "config/gaggles/example/workflows/default-implement.yaml"), parent)
		path := filepath.Join(root, "config/gaggles/example/goobers/coder/goober.yaml")
		writeFileContent(t, path, strings.Replace(readFileContent(t, path), "harness: copilot", "harness: claude-code", 1))
		path = filepath.Join(root, "instance.yaml")
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(readFileContent(t, path)), &doc); err != nil {
			t.Fatal(err)
		}
		delete(doc, "engine")
		delete(doc, "runners")
		delete(doc, "runner")
		grants, _ := doc["credentials"].([]any)
		doc["credentials"] = append(grants, map[string]any{"capability": "agent:model", "harness": "claude-code", "token": map[string]any{"env": "QUALIFICATION_MODEL_TOKEN"}})
		doc["cost"] = map[string]any{"enabled": false}
		data, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		writeFileContent(t, path, string(data))
	})
}

// Real queue, scheduler, local executors, snapshots, journal and process custody;
// only the model and the remote Git origin are replaced with local fixtures.
func TestIntegrationTrustedLocalChildJourney(t *testing.T) {
	testdep.Require(t, "git", "sh")
	for _, action := range []string{"merge", "replace", "discard"} {
		t.Run(action, func(t *testing.T) { qualifyTrustedLocalJourney(t, action) })
	}
}

func qualifyTrustedLocalJourney(t *testing.T, action string) {
	bin := t.TempDir()
	installQualificationParentProbe(t, bin)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOOBERS_GITHUB_TOKEN", "local-git-fixture-only")
	f := trustedLocalJourneyFixture(t)
	source := t.TempDir()
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	writeFileContent(t, filepath.Join(source, "source.txt"), "base\n")
	recoveryCLIGit(t, source, "add", ".")
	recoveryCLIGit(t, source, "commit", "-m", "base")
	originalClone := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return source, nil }
	t.Cleanup(func() { repoCloneURL = originalClone })
	instanceLog, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })
	registry := newDaemonRunnerRegistry()
	dispatch := newDaemonTriggerService()
	triggers, _, cancels, err := newDaemonCoordinationServices(f.layout, dispatch, registry, instanceLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = triggers.queue.Close(); _ = cancels.receipts.Close() })
	s := newDaemonCredentialService(f.layout, f.cfg, nil, journal.NewRegistryScrubber(), instanceLog).withStageGrants(f.layout.Root, "127.0.0.1:8080", false)
	t.Cleanup(func() { unregisterDaemonStageGrants(f.layout.Root, s) })
	s.childDispatch = dispatch
	s.Replace(credentialPlaneDefinitionsFromSet(f.applied))
	if err := s.enableChildWorkflows(triggers.queue, f.applied); err != nil {
		t.Fatal(err)
	}
	s.installTrustedLocalChildren(childFactoryJournals(t, s))
	var calls atomic.Int32
	var runID string
	previous := newAgenticAdapter
	newAgenticAdapter = func(string, map[string]string) harness.Adapter {
		return &harnesstest.FakeAdapter{Act: func(ctx context.Context, req harness.RunRequest) error {
			n := calls.Add(1)
			t.Log("parent invocation", n)
			if !credentials.TrustedLocalProcess(ctx) || req.ChildWorkflows == nil {
				return fmt.Errorf("parent missing delegated local authority")
			}
			// A real managed process establishes the same quiescence owner as a CLI.
			result, err := (harness.ExecProcessRunner{}).Run(ctx, harness.ProcessRequest{Command: []string{"/bin/sh", "-c", "printf 'parent\\n' > parent.txt"}, Dir: req.Workspace, Env: credentials.TrustedLocalBaseEnvironment(ctx, os.Environ()), Timeout: 5 * time.Second})
			if err != nil || result.ExitCode != 0 {
				return fmt.Errorf("parent process: %v %+v", err, result)
			}
			token := req.ChildWorkflows.BearerToken
			switch n {
			case 1:
				proposal := strings.Replace(childValidationProposal, "      goal:", "      workspace: repo\n      goal:", 1)
				proposal = strings.Replace(proposal, `run: {command: ["true"]}`, `run: {command: ["sh", "-c", "printf 'child\\n' > child.txt"]}`, 1)
				if _, err := s.children.HTTPService().StartChildWorkflow(ctx, token, runID, "inspect", []byte(proposal)); err != nil {
					return err
				}
				<-ctx.Done()
				return ctx.Err()
			case 2:
				status, err := s.children.HTTPService().ChildWorkflowStatus(ctx, token, runID, "inspect")
				if err != nil {
					return err
				}
				if status.ResultRef == "" {
					return fmt.Errorf("no terminal result: %+v", status)
				}
				if _, err := s.children.HTTPService().ResolveChildWorkflow(ctx, token, runID, apicontract.ChildWorkflowResolveRequest{InvocationKey: "inspect", Action: action, ResultRef: status.ResultRef}); err != nil {
					return err
				}
				<-ctx.Done()
				return ctx.Err()
			case 3:
				_, childErr := os.Stat(filepath.Join(req.Workspace, "child.txt"))
				if (action == "discard") != os.IsNotExist(childErr) {
					return fmt.Errorf("%s returned unexpected child files: %v", action, childErr)
				}
				return harnesstest.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			default:
				return fmt.Errorf("unexpected parent invocation %d", n)
			}
		}}
	}
	t.Cleanup(func() { newAgenticAdapter = previous })
	store, err := executionGenerationStore(f.layout)
	if err != nil {
		t.Fatal(err)
	}
	retainer := &configgeneration.Retainer{Store: store}
	t.Cleanup(func() { _ = retainer.Close() })
	var wg sync.WaitGroup
	build := func(layout instance.Layout, generation string, set *instance.ConfigSet, report *validate.Report) (*schedulerDefinitions, error) {
		return buildSchedulerDefinitions(schedulerDefinitionsInput{Layout: layout, Config: f.cfg, Definitions: set, Validation: report, RunnerRegistry: registry, ProviderQuota: localscheduler.NewProviderQuotaState(), Generations: []*configgeneration.Retainer{retainer}, PinnedGeneration: generation, SharedRegistry: s.shared, InstanceLog: instanceLog, WaitGroup: &wg, TrustedLocalChildRuntime: len(set.Workflows) == 1 && set.Workflows[0].Name == "generated-check"})
	}
	set, report, err := loadConfigDirectory(f.retainedPath)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := build(f.layout, f.parent.ConfigGeneration, set, report)
	if err != nil {
		t.Fatal(err)
	}
	setup := &schedulerSetup{RunnerRegistry: registry, ChildRuntime: childRuntimeBuilderFor(f.layout, retainer, f.cfg, build)}
	if err := s.installQueuedChildren(setup, triggers, &wg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	scheduler := localscheduler.New(definitions.Entries, instanceLog, localscheduler.WithInstanceRunConditions(f.cfg.RunConditions.MaxParallelRuns, f.cfg.RunConditions.WorkflowBudgets, f.cfg.RunConditions.WorkflowDailyBudgets))
	dispatch.AttachScheduler(scheduler)
	dispatch.AttachDispatchContext(ctx)
	t.Cleanup(func() { cancel(); scheduler.Wait(); wg.Wait() })
	accepted, err := triggers.Trigger(ctx, httpapi.TriggerRequest{Gaggle: "example", Workflow: f.parent.Workflow, RequestID: "local-child-journey", Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	runID = strings.TrimPrefix(accepted.AcceptanceID, "trigger-")
	for {
		if err := triggers.Drain(ctx); err != nil {
			t.Fatal(err)
		}

		receipt, receiptErr := triggers.queue.Get(ctx, accepted.AcceptanceID, "test")
		if receiptErr != nil {
			t.Fatal(receiptErr)
		}
		if receipt.State == triggerqueue.Rejected {
			t.Fatalf("parent trigger refused: %+v", receipt)
		}
		if dir, err := f.layout.FindRunDir(runID); err == nil {
			reader, err := journal.OpenReadOnly(dir)
			if err != nil {
				t.Fatal(err)
			}
			phase, err := reader.PhaseBounded(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if phase != journal.PhaseRunning {
				if phase != journal.PhaseCompleted {
					events, _ := reader.Events()
					for _, e := range events {
						if e.Error != nil || e.TerminalCause != nil {
							t.Logf("%s: %+v %+v", e.Type, e.Error, e.TerminalCause)
						}
					}
					t.Fatalf("parent phase=%s calls=%d", phase, calls.Load())
				}
				children, err := triggers.queue.Children(ctx, triggerqueue.ChildParent{Gaggle: "example", ParentRunID: runID}, "", 10)
				if err != nil || len(children) != 1 || children[0].State != triggerqueue.ChildCompleted {
					t.Fatalf("children=%+v err=%v", children, err)
				}
				if calls.Load() != 3 {
					t.Fatalf("parent calls=%d", calls.Load())
				}
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}
