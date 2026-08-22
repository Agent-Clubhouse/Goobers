package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

func TestUpReloadsValidConfigAndRejectsInvalidEdit(t *testing.T) {
	previousReloadInterval := configReloadInterval
	previousDelegationInterval := delegationSweepInterval
	configReloadInterval = 20 * time.Millisecond
	delegationSweepInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		configReloadInterval = previousReloadInterval
		delegationSweepInterval = previousDelegationInterval
	})

	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	address := freeLoopbackAddress(t)
	setAPIListenAddress(t, root, address)
	manifestPath := filepath.Join(layout.ConfigDir(), "manifest.yaml")
	gagglePath := filepath.Join(layout.ConfigDir(), "gaggles", "example", "gaggle.yaml")
	workflowPath := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")
	mirrorPath := t.TempDir()
	gaggle, err := os.ReadFile(gagglePath)
	if err != nil {
		t.Fatal(err)
	}
	gaggle = append(gaggle, []byte("  outboxMirrorPath: "+mirrorPath+"\n")...)
	if err := os.WriteFile(gagglePath, gaggle, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := &daemonStartedWriter{started: make(chan struct{})}
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runUpContext(ctx, []string{"--quiet", "--watch-config", root}, started, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-daemonDone:
			if code != 0 {
				t.Errorf("daemon exit code = %d", code)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	select {
	case <-started.started:
	case code := <-daemonDone:
		t.Fatalf("daemon exited before startup with code %d", code)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	initialHealth := readDaemonHealth(t, address)
	if initialHealth.Instance.Name != "example" || initialHealth.Instance.Environment != apiv1.EnvironmentDev {
		t.Fatalf("initial instance = %+v", initialHealth.Instance)
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	reloadedManifest := strings.Replace(
		string(manifest),
		"    name: example\n    environment: dev",
		"    name: reloaded-example\n    environment: staging",
		1,
	)
	if reloadedManifest == string(manifest) {
		t.Fatal("manifest identity fixture not found")
	}
	if err := os.WriteFile(manifestPath, []byte(reloadedManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded := waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloaded, 1)
	oldDigest, oldOK := reloaded.Runner["oldDigest"].(string)
	newDigest, newOK := reloaded.Runner["newDigest"].(string)
	if !oldOK || !newOK || oldDigest == "" || newDigest == "" || oldDigest == newDigest {
		t.Fatalf("config.reloaded digests = %+v, want distinct old/new digests", reloaded.Runner)
	}
	reloadedHealth := waitForDaemonHealth(t, address, "reloaded-example", apiv1.EnvironmentStaging)
	if !reloadedHealth.Freshness.DefinitionsLoadedAt.After(initialHealth.Freshness.DefinitionsLoadedAt) {
		t.Fatalf(
			"definitionsLoadedAt = %s, want after startup value %s",
			reloadedHealth.Freshness.DefinitionsLoadedAt,
			initialHealth.Freshness.DefinitionsLoadedAt,
		)
	}

	reloadedWorkflow := `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: reloaded-implement
spec:
  gaggle: example
  triggers:
    - type: schedule
      schedule: "@every 24h"
  start: local-ci
  tasks:
    - name: local-ci
      type: deterministic
      goal: run a no-op local command
      run:
        command: ["sh", "-c", "mkdir -p reports && printf reloaded > reports/report.txt"]
      outbox:
        - reports/report.txt
      next: approval
    - name: finish
      type: deterministic
      goal: finish after approval
      run:
        command: ["true"]
  gates:
    - name: approval
      evaluator: human
      human: {}
      branches:
        pass: finish
        fail: "@abort"
`
	if err := os.WriteFile(workflowPath, []byte(reloadedWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloaded, 2)
	// The journal event says a reload happened; it does not say the scheduler
	// has swapped in the new definitions, and `run` resolves the workflow name
	// through the scheduler. Waiting on definitionsLoadedAt closes that gap —
	// without it this test fails on loaded runners with
	// `localscheduler: unknown workflow "reloaded-implement"`.
	waitForDefinitionsReload(t, address, reloadedHealth.Freshness.DefinitionsLoadedAt)
	stdout := waitForRunnableWorkflow(t, root, "reloaded-implement")
	runID := runIDFromRunStdout(t, stdout)
	mirrored := waitForConfigValue(t, "gaggle outbox mirror after reload", func() ([]byte, bool) {
		data, err := os.ReadFile(filepath.Join(mirrorPath, runID, "local-ci", "attempt-1", "reports", "report.txt"))
		if errors.Is(err, os.ErrNotExist) {
			return nil, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return data, true
	})
	if string(mirrored) != "reloaded" {
		t.Fatalf("mirrored outbox = %q, want reloaded", mirrored)
	}
	runDir := filepath.Join(layout.ForGaggle("example").RunsDir(), runID)
	deadline := time.Now().Add(10 * time.Second)
	for {
		reader, err := journal.OpenRead(runDir)
		if err == nil {
			events, readErr := reader.Events()
			if readErr != nil {
				t.Fatal(readErr)
			}
			paused := false
			for _, event := range events {
				if event.Type == journal.EventGatePaused && event.Gate == "approval" {
					paused = true
					break
				}
			}
			if paused {
				break
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("post-reload run %s did not pause at approval", runID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	code, stdout, stderr := runArgs(t, "approve", "--actor=config-reloader", runID, "approval", root)
	if code != 0 {
		t.Fatalf("approve post-reload run: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	if err := os.WriteFile(workflowPath, []byte("kind: Workflow\nmetadata: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rejected := waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloadRejected, 1)
	if rejected.Error == nil || rejected.Error.Code != "config_reload_rejected" || rejected.Error.Message == "" {
		t.Fatalf("config.reload.rejected error = %+v", rejected.Error)
	}

	code, stdout, stderr = runArgs(t, "run", "--no-wait", "reloaded-implement", root)
	if code != 0 {
		t.Fatalf("last-known-good workflow unavailable after rejected edit: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestUpReconcilesGitWorkflowSourceAndRetainsLastKnownGood(t *testing.T) {
	previousReloadInterval := configReloadInterval
	configReloadInterval = time.Hour
	t.Cleanup(func() { configReloadInterval = previousReloadInterval })

	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	address := freeLoopbackAddress(t)
	setAPIListenAddress(t, root, address)

	sourceRepo := filepath.Join(t.TempDir(), "workflow-source")
	if err := os.CopyFS(sourceRepo, os.DirFS(layout.ConfigDir())); err != nil {
		t.Fatal(err)
	}
	runGitT(t, sourceRepo, "init", "-b", "main")
	runGitT(t, sourceRepo, "config", "user.name", "config source")
	runGitT(t, sourceRepo, "config", "user.email", "config-source@example.test")
	runGitT(t, sourceRepo, "add", ".")
	runGitT(t, sourceRepo, "commit", "-m", "initial config")

	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.WorkflowSource = &instance.WorkflowSource{
		Kind: instance.WorkflowSourceKindGit,
		Path: sourceRepo,
	}
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := &daemonStartedWriter{started: make(chan struct{})}
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runUpContext(ctx, []string{"--quiet", root}, started, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-daemonDone:
			if code != 0 {
				t.Errorf("daemon exit code = %d", code)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	select {
	case <-started.started:
	case code := <-daemonDone:
		t.Fatalf("daemon exited before startup with code %d", code)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	workflowPath := filepath.Join(sourceRepo, "gaggles", "example", "workflows", "default-implement.yaml")
	valid := strings.Replace(deterministicWorkflowYAML, "name: default-implement", "name: reconciled-implement", 1)
	if err := os.WriteFile(workflowPath, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, sourceRepo, "add", ".")
	runGitT(t, sourceRepo, "commit", "-m", "valid config")
	waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloaded, 1)
	waitForRunnableWorkflow(t, root, "reconciled-implement")

	if err := os.WriteFile(workflowPath, []byte("kind: Workflow\nmetadata: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, sourceRepo, "add", ".")
	runGitT(t, sourceRepo, "commit", "-m", "invalid config")
	rejected := waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloadRejected, 1)
	if rejected.Error == nil || rejected.Error.Code != "config_reload_rejected" {
		t.Fatalf("config.reload.rejected error = %+v", rejected.Error)
	}
	waitForRunnableWorkflow(t, root, "reconciled-implement")
}

func TestUpAcceptsPushWebhookForGitWorkflowSource(t *testing.T) {
	previousReloadInterval := configReloadInterval
	configReloadInterval = time.Hour
	t.Cleanup(func() { configReloadInterval = previousReloadInterval })

	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	setAPIListenAddress(t, root, freeLoopbackAddress(t))

	sourceRepo := filepath.Join(t.TempDir(), "workflow-source")
	if err := os.CopyFS(sourceRepo, os.DirFS(layout.ConfigDir())); err != nil {
		t.Fatal(err)
	}
	runGitT(t, sourceRepo, "init", "-b", "main")
	runGitT(t, sourceRepo, "config", "user.name", "config source")
	runGitT(t, sourceRepo, "config", "user.email", "config-source@example.test")
	runGitT(t, sourceRepo, "add", ".")
	runGitT(t, sourceRepo, "commit", "-m", "initial config")

	const (
		secretEnv = "GOOBERS_TEST_CONFIG_RECONCILE_WEBHOOK_SECRET"
		secret    = "config-reconcile-webhook-secret"
	)
	webhookAddress := freeLoopbackAddress(t)
	t.Setenv(secretEnv, secret)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.WorkflowSource = &instance.WorkflowSource{
		Kind: instance.WorkflowSourceKindGit,
		Path: sourceRepo,
	}
	cfg.Webhook.Listen = webhookAddress
	cfg.Webhook.Secret.Env = secretEnv
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := &daemonStartedWriter{started: make(chan struct{})}
	daemonDone := make(chan int, 1)
	var stderr bytes.Buffer
	go func() {
		daemonDone <- runUpContext(ctx, []string{"--quiet", root}, started, &stderr)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-daemonDone:
			if code != 0 {
				t.Errorf("daemon exit code = %d", code)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	select {
	case <-started.started:
	case code := <-daemonDone:
		t.Fatalf("daemon exited before startup with code %d: %s", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	workflowPath := filepath.Join(sourceRepo, "gaggles", "example", "workflows", "default-implement.yaml")
	valid := strings.Replace(deterministicWorkflowYAML, "name: default-implement", "name: webhook-reconciled-implement", 1)
	valid = strings.Replace(
		valid,
		"    - type: schedule\n      schedule: \"@every 24h\"\n",
		"    - type: webhook\n      events: [issues]\n",
		1,
	)
	if err := os.WriteFile(workflowPath, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, sourceRepo, "add", ".")
	runGitT(t, sourceRepo, "commit", "-m", "add first webhook trigger")

	if status := postWebhook(t, webhookAddress, secret, "push", "config-push-1", []byte(`{}`)); status != http.StatusAccepted {
		t.Fatalf("push webhook status = %d, want %d", status, http.StatusAccepted)
	}
	waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloaded, 1)
	waitForRunnableWorkflow(t, root, "webhook-reconciled-implement")

	withoutTrigger := strings.Replace(
		valid,
		"    - type: webhook\n      events: [issues]\n",
		"    - type: schedule\n      schedule: \"@every 24h\"\n",
		1,
	)
	if err := os.WriteFile(workflowPath, []byte(withoutTrigger), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, sourceRepo, "add", ".")
	runGitT(t, sourceRepo, "commit", "-m", "remove last webhook trigger")

	if status := postWebhook(t, webhookAddress, secret, "push", "config-push-2", []byte(`{}`)); status != http.StatusAccepted {
		t.Fatalf("push webhook status = %d, want %d", status, http.StatusAccepted)
	}
	waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloaded, 2)
}

func TestConfigSourceReconcilerWakesWithoutPolling(t *testing.T) {
	previousReloadInterval := configReloadInterval
	configReloadInterval = time.Hour
	t.Cleanup(func() { configReloadInterval = previousReloadInterval })

	wake := make(chan struct{}, 1)
	reconciled := make(chan struct{}, 2)
	loop := &configSourceReconciler{
		source: instance.WorkflowSource{Kind: instance.WorkflowSourceKindLocalDir},
		errors: &sweepErrorReporter{},
		wake:   wake,
		reconcile: func(context.Context, time.Time) error {
			reconciled <- struct{}{}
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()
	select {
	case <-reconciled:
	case <-time.After(time.Second):
		t.Fatal("initial reconcile did not run")
	}

	wake <- struct{}{}
	select {
	case <-reconciled:
	case <-time.After(time.Second):
		t.Fatal("hook wake did not reconcile")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("reconciler stopped with error: %v", err)
	}
}

// TestConfigSourceReconcilerObservesLinkedWorktreeCommit pins the fix for a
// bug in watchGitRef: for a linked worktree, ".git" is a file pointing at the
// per-worktree admin directory (.git/worktrees/<name>), which holds HEAD and
// the index, but branch refs (refs/heads/..., packed-refs) live in the
// common Git directory recorded by that admin directory's "commondir" file.
// Watching only the per-worktree admin directory can observe "git add"
// staging but misses the ref update a commit finalizes there. configReloadInterval
// is pinned to an hour so the ticker cannot be the one driving reconciliation:
// the only way this test's reconcile can fire before it times out is the
// fsnotify watcher correctly resolving and watching the common directory.
func TestConfigSourceReconcilerObservesLinkedWorktreeCommit(t *testing.T) {
	previousReloadInterval := configReloadInterval
	configReloadInterval = time.Hour
	t.Cleanup(func() { configReloadInterval = previousReloadInterval })

	mainRepo := t.TempDir()
	runGitT(t, mainRepo, "init", "-b", "main")
	runGitT(t, mainRepo, "config", "user.name", "config source")
	runGitT(t, mainRepo, "config", "user.email", "config-source@example.test")
	if err := os.WriteFile(filepath.Join(mainRepo, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, mainRepo, "add", "README.md")
	runGitT(t, mainRepo, "commit", "-m", "seed")

	worktree := filepath.Join(t.TempDir(), "linked-worktree")
	runGitT(t, mainRepo, "worktree", "add", "-b", "tracked", worktree, "main")

	// A single commit can fan out into many inotify events on Linux (index
	// lock create/rename, packed-refs rewrite, loose-ref create, etc. —
	// macOS's FSEvents backend coalesces far more aggressively), so the
	// reconcile hook must never block Run's event loop: a blocking send
	// here deadlocks Run inside the select case that called it, and cancel
	// can't unstick it because ctx.Done() is only observed back at the
	// select. Use a generously buffered channel plus a non-blocking send
	// and drain-then-wait on the receive side instead of counting exact
	// reconcile calls.
	reconciled := make(chan struct{}, 64)
	loop := &configSourceReconciler{
		source: instance.WorkflowSource{
			Kind: instance.WorkflowSourceKindGit,
			Path: worktree,
			Ref:  "tracked",
		},
		errors: &sweepErrorReporter{},
		wake:   make(chan struct{}),
		reconcile: func(context.Context, time.Time) error {
			select {
			case reconciled <- struct{}{}:
			default:
			}
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("reconciler did not stop after cancel")
		}
	})

	drainReconciled := func() {
		for {
			select {
			case <-reconciled:
			default:
				return
			}
		}
	}

	select {
	case <-reconciled:
	case <-time.After(5 * time.Second):
		t.Fatal("initial reconcile did not run")
	}
	// The initial reconcile can be followed by extra watcher-driven signals
	// from setup (e.g. the watcher's own directory Add calls). Drain them so
	// the wait below only observes signals caused by the commit itself.
	drainReconciled()

	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, worktree, "add", "README.md")
	runGitT(t, worktree, "commit", "-m", "update in linked worktree")

	select {
	case <-reconciled:
	case <-time.After(5 * time.Second):
		t.Fatal("commit in linked worktree did not trigger a watcher-driven reconcile")
	}
}

// TestConfigReloaderPollSerializedAcrossWatchConfigAndApplySweep pins #459's
// fix for a real concurrency bug QA-2 caught on review of PR #2131:
// configReloader was only ever safe because exactly one goroutine called
// poll() (Run's own ticker, gated behind --watch-config) — but #459 makes
// the reloader always-constructed and its own apply-sweep ticker
// (unconditional) also calls into it via pollOnce. An operator running
// `goobers up --watch-config` and `goobers apply` concurrently would
// otherwise race on every field configReloader.poll mutates. Runs both
// callers concurrently and hard under `go test -race` to prove they're now
// serialized by configReloader.mu rather than merely "usually fine."
func TestConfigReloaderPollSerializedAcrossWatchConfigAndApplySweep(t *testing.T) {
	previousReloadInterval := configReloadInterval
	previousDelegationInterval := delegationSweepInterval
	configReloadInterval = 5 * time.Millisecond
	delegationSweepInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		configReloadInterval = previousReloadInterval
		delegationSweepInterval = previousDelegationInterval
	})

	root := initDeterministicDemo(t)
	address := freeLoopbackAddress(t)
	setAPIListenAddress(t, root, address)

	ctx, cancel := context.WithCancel(context.Background())
	started := &daemonStartedWriter{started: make(chan struct{})}
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runUpContext(ctx, []string{"--quiet", "--watch-config", root}, started, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-daemonDone:
			if code != 0 {
				t.Errorf("daemon exit code = %d", code)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	select {
	case <-started.started:
	case code := <-daemonDone:
		t.Fatalf("daemon exited before startup with code %d", code)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	// --watch-config's own ticker is now independently polling the same
	// configReloader every 5ms. Hammer `goobers apply` from several
	// concurrent goroutines at the same time — before the r.mu fix this
	// raced on configReloader's fields under `go test -race`.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				runApply([]string{root}, io.Discard, io.Discard)
			}
		}()
	}
	wg.Wait()
}

func TestUpReloadsResolvedGooberContentForNextRun(t *testing.T) {
	previousReloadInterval := configReloadInterval
	previousDelegationInterval := delegationSweepInterval
	configReloadInterval = 20 * time.Millisecond
	delegationSweepInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		configReloadInterval = previousReloadInterval
		delegationSweepInterval = previousDelegationInterval
	})

	root := initDemo(t)
	layout := instance.NewLayout(root)
	address := freeLoopbackAddress(t)
	setAPIListenAddress(t, root, address)
	workflowPath := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")
	writeFixture(t, workflowPath, `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: manual
  start: implement
  tasks:
    - name: implement
      type: agentic
      goober: coder
      goal: Complete the fixture task.
      capabilities:
        - agent:model
`)
	// Scoped (gaggles/example/skills/implement) always wins over the shared
	// instance-level fallback (skillPackagePaths) — and the starter scaffold
	// now ships a scoped implement package by default (SKILL002 fix) — so
	// the digest transition below must be authored at the scoped path, or it
	// is masked by that (untouched) scoped package.
	skillPath := filepath.Join(layout.ConfigDir(), "gaggles", "example", "skills", "implement", "SKILL.md")
	writeFixture(t, skillPath, "# Original implementation skill\n")

	fixtureRepo := newDaemonFixtureRepo(t)
	previousRepoCloneURL := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil }
	previousAdapter := newAgenticAdapter
	newAgenticAdapter = func(string, map[string]string) harness.Adapter {
		return &harnesstest.FakeAdapter{Act: func(_ context.Context, req harness.RunRequest) error {
			return harnesstest.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Summary: "completed fixture task",
			})
		}}
	}
	t.Cleanup(func() {
		repoCloneURL = previousRepoCloneURL
		newAgenticAdapter = previousAdapter
	})

	ctx, cancel := context.WithCancel(context.Background())
	started := &daemonStartedWriter{started: make(chan struct{})}
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runUpContext(ctx, []string{"--quiet", "--watch-config", root}, started, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-daemonDone:
			if code != 0 {
				t.Errorf("daemon exit code = %d", code)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	select {
	case <-started.started:
	case code := <-daemonDone:
		t.Fatalf("daemon exited before startup with code %d", code)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	runIdentity := func() journal.RunIdentity {
		t.Helper()
		code, stdout, stderr := runArgs(t, "run", "default-implement", root)
		if code != 0 {
			t.Fatalf("run default-implement: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		runID := runIDFromRunStdout(t, stdout)
		reader, err := journal.OpenRead(filepath.Join(layout.ForGaggle("example").RunsDir(), runID))
		if err != nil {
			t.Fatal(err)
		}
		identity, err := reader.Identity()
		if err != nil {
			t.Fatal(err)
		}
		return identity
	}

	initialHealth := readDaemonHealth(t, address)
	before := runIdentity()
	instructionsPath := filepath.Join(layout.ConfigDir(), "gaggles", "example", "goobers", "coder", "instructions.md")
	if err := os.WriteFile(instructionsPath, []byte("# Reloaded coder instructions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloaded, 1)
	waitForDefinitionsReload(t, address, initialHealth.Freshness.DefinitionsLoadedAt)
	after := runIdentity()

	if before.GooberDigest == "" || after.GooberDigest == "" || before.GooberDigest == after.GooberDigest {
		t.Fatalf("goober digests before=%q after=%q, want distinct non-empty values", before.GooberDigest, after.GooberDigest)
	}
	if before.WorkflowDigest != after.WorkflowDigest {
		t.Fatalf("workflow digest changed after instructions edit: before=%q after=%q", before.WorkflowDigest, after.WorkflowDigest)
	}

	instructionHealth := readDaemonHealth(t, address)
	if err := os.WriteFile(skillPath, []byte("# Reloaded implementation skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloaded, 2)
	waitForDefinitionsReload(t, address, instructionHealth.Freshness.DefinitionsLoadedAt)
	afterSkill := runIdentity()
	if after.GooberDigest == afterSkill.GooberDigest {
		t.Fatalf("goober digest did not change after skill edit: %q", afterSkill.GooberDigest)
	}
	if after.WorkflowDigest != afterSkill.WorkflowDigest {
		t.Fatalf("workflow digest changed after skill edit: before=%q after=%q", after.WorkflowDigest, afterSkill.WorkflowDigest)
	}
}

func readDaemonHealth(t *testing.T, address string) readservice.Health {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Get("http://" + address + httpapi.HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		t.Fatalf("health status = %d", response.StatusCode)
	}
	var health readservice.Health
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return health
}

func waitForConfigValue[T any](t *testing.T, description string, read func() (T, bool)) T {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		if value, ready := read(); ready {
			return value
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatalf("timed out waiting for %s", description)
		}
	}
}

func waitForDaemonHealth(t *testing.T, address, name string, environment apiv1.Environment) readservice.Health {
	t.Helper()
	return waitForConfigValue(t, "health identity "+name+"/"+string(environment), func() (readservice.Health, bool) {
		health := readDaemonHealth(t, address)
		return health, health.Instance.Name == name && health.Instance.Environment == environment
	})
}

// waitForRunnableWorkflow runs a workflow by name, retrying only while the
// scheduler reports it unknown.
//
// definitionsLoadedAt advancing means the daemon reloaded configuration; it
// does not mean the scheduler has registered a workflow that did not exist
// before. This test renames default-implement to reloaded-implement, so it
// depends on registration of a NEW name rather than on refreshed content — a
// strictly later step. The sibling test that only changes goober content is
// fully covered by the definitionsLoadedAt wait, which is why that one has
// never flaked and this one still did after the wait was added (#1784, and the
// recurrence on PR #1818).
//
// Retrying the operation itself waits for exactly the condition asserted. Only
// the "unknown workflow" stderr is retried: any other non-zero exit fails
// immediately, so a genuine regression still surfaces rather than being spun on
// until the deadline.
func waitForRunnableWorkflow(t *testing.T, root, workflow string) string {
	t.Helper()
	return waitForConfigValue(t, workflow+" to become runnable", func() (string, bool) {
		code, stdout, stderr := runArgs(t, "run", "--no-wait", workflow, root)
		if code == 0 {
			return stdout, true
		}
		if !strings.Contains(stderr, "unknown workflow") {
			t.Fatalf("run %s: code=%d stdout=%q stderr=%q", workflow, code, stdout, stderr)
		}
		return "", false
	})
}

func waitForDefinitionsReload(t *testing.T, address string, loadedAt time.Time) {
	t.Helper()
	waitForConfigValue(t, "definitions loaded after "+loadedAt.String(), func() (struct{}, bool) {
		return struct{}{}, readDaemonHealth(t, address).Freshness.DefinitionsLoadedAt.After(loadedAt)
	})
}

func TestBuildSchedulerSetupRejectsConfigChangedDuringStartup(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	workflowPath := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")

	previousLoader := loadConfigDirectory
	loadConfigDirectory = func(dir string) (*instance.ConfigSet, *validate.Report, error) {
		set, report, err := instance.LoadConfigDir(dir)
		if err != nil {
			return set, report, err
		}
		changed := strings.Replace(deterministicWorkflowYAML, "name: default-implement", "name: changed-during-startup", 1)
		if err := os.WriteFile(workflowPath, []byte(changed), 0o644); err != nil {
			return nil, report, err
		}
		return set, report, nil
	}
	t.Cleanup(func() { loadConfigDirectory = previousLoader })

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), layout, &wg)
	if setup != nil {
		setup.Shutdown(context.Background())
	}
	if err == nil || !strings.Contains(err.Error(), "config directory changed during daemon setup") {
		t.Fatalf("buildSchedulerSetup error = %v, want changed-during-setup refusal", err)
	}
}

func waitForConfigEvent(t *testing.T, schedulerDir string, eventType journal.EventType, count int) journal.Event {
	t.Helper()
	return waitForConfigValue(t, string(eventType)+" event", func() (journal.Event, bool) {
		events, err := journal.ReadInstanceLog(schedulerDir)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, event := range events {
			if event.Type != eventType {
				continue
			}
			seen++
			if seen == count {
				return event, true
			}
		}
		return journal.Event{}, false
	})
}

func TestConfigDirectoryDigestOnlyTracksLoadedConfigAndAssets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("kind: A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gooberDir := filepath.Join(root, "gaggles", "example", "goobers", "coder")
	if err := os.MkdirAll(gooberDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gooberDir, "goober.yaml"), []byte(`kind: Goober
spec:
  gaggle: example
  instructions: instructions.md
  skills:
    - implement
`), 0o644); err != nil {
		t.Fatal(err)
	}
	instructionsPath := filepath.Join(gooberDir, "instructions.md")
	if err := os.WriteFile(instructionsPath, []byte("# Original instructions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(filepath.Dir(root), "skills", "implement", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("# Original skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseline, err := configDirectoryDigest(root)
	if err != nil {
		t.Fatal(err)
	}

	// Non-config churn must NOT move the digest: a README, editor swap/backup
	// files, and a .git worktree are all outside the loader's surface, so
	// touching them must not trigger a reload or a false rejection.
	noise := map[string]string{
		"README.md":          "# docs\n",
		".manifest.yaml.swp": "vim-swap-garbage",
		"manifest.yaml~":     "editor backup",
		"4913":               "vim probe file",
		".git/index":         "git internals",
		".git/HEAD":          "ref: refs/heads/main\n",
		"config.json":        "{}",
		"gaggles/example/goobers/coder/README.md": "# unrelated docs\n",
	}
	for name, content := range noise {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := configDirectoryDigest(root); err != nil {
		t.Fatal(err)
	} else if got != baseline {
		t.Fatalf("non-config churn changed digest: got %s, want %s", got, baseline)
	}

	if err := os.WriteFile(instructionsPath, []byte("# Updated instructions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withInstructions, err := configDirectoryDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if withInstructions == baseline {
		t.Fatalf("referenced instruction edit did not change digest: %s", withInstructions)
	}

	if err := os.WriteFile(skillPath, []byte("# Updated skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withSkill, err := configDirectoryDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if withSkill == withInstructions {
		t.Fatalf("referenced skill edit did not change digest: %s", withSkill)
	}
	referencePath := filepath.Join(filepath.Dir(skillPath), "references", "cases.md")
	if err := os.MkdirAll(filepath.Dir(referencePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(referencePath, []byte("Handle the retry case.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withSkillReference, err := configDirectoryDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if withSkillReference == withSkill {
		t.Fatalf("referenced skill support-file addition did not change digest: %s", withSkillReference)
	}
	scopedSkillPath := filepath.Join(root, "gaggles", "example", "skills", "implement", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(scopedSkillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scopedSkillPath, []byte("# Gaggle skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withScopedSkill, err := configDirectoryDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if withScopedSkill == withSkillReference {
		t.Fatalf("gaggle skill addition did not change digest: %s", withScopedSkill)
	}
	if err := os.WriteFile(skillPath, []byte("# Ignored shared update\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := configDirectoryDigest(root); err != nil {
		t.Fatal(err)
	} else if got != withScopedSkill {
		t.Fatalf("shadowed shared skill changed digest: got %s, want %s", got, withScopedSkill)
	}
	undeclaredSkillPath := filepath.Join(root, "gaggles", "example", "skills", "undeclared", "support.yaml")
	if err := os.MkdirAll(filepath.Dir(undeclaredSkillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(undeclaredSkillPath, []byte("cases:\n  - ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := configDirectoryDigest(root); err != nil {
		t.Fatal(err)
	} else if got != withScopedSkill {
		t.Fatalf("undeclared gaggle skill changed digest: got %s, want %s", got, withScopedSkill)
	}
	scopedSupportPath := filepath.Join(filepath.Dir(scopedSkillPath), "support.yaml")
	if err := os.WriteFile(scopedSupportPath, []byte("cases:\n  - retry\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withScopedSupport, err := configDirectoryDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if withScopedSupport == withScopedSkill {
		t.Fatalf("declared gaggle skill support file did not change digest: %s", withScopedSupport)
	}

	asset := filepath.Join(root, "gaggles", "example", "goobers", "coder", "assets", ".hidden", "reference.txt")
	if err := os.MkdirAll(filepath.Dir(asset), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asset, []byte("static reference"), 0o644); err != nil {
		t.Fatal(err)
	}
	withAsset, err := configDirectoryDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if withAsset == withScopedSupport {
		t.Fatalf("asset addition did not change digest: %s", withAsset)
	}

	// A real change to a loaded config file MUST move the digest.
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("kind: B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := configDirectoryDigest(root); err != nil {
		t.Fatal(err)
	} else if got == withAsset {
		t.Fatalf("config edit did not change digest: %s", got)
	}
}

func TestConfigDirectoryDigestTracksParentRelativeInstructions(t *testing.T) {
	root := t.TempDir()
	gooberDir := filepath.Join(root, "gaggles", "example", "goobers", "coder")
	if err := os.MkdirAll(gooberDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gooberDir, "goober.yaml"), []byte(`kind: Goober
spec:
  instructions: ../shared.md
`), 0o644); err != nil {
		t.Fatal(err)
	}
	instructionsPath := filepath.Join(filepath.Dir(gooberDir), "shared.md")
	if err := os.WriteFile(instructionsPath, []byte("# Original instructions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseline, err := configDirectoryDigest(root)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(instructionsPath, []byte("# Updated instructions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := configDirectoryDigest(root); err != nil {
		t.Fatal(err)
	} else if got == baseline {
		t.Fatalf("parent-relative instruction edit did not change digest: %s", got)
	}
}

func TestConfigDirectoryDigestRejectsUnsafeAssets(t *testing.T) {
	tests := map[string]func(*testing.T, string){
		"symlink": func(t *testing.T, assets string) {
			if err := os.Mkdir(assets, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(assets, "reference")); err != nil {
				t.Skipf("symlinks unsupported: %v", err)
			}
		},
		"special file": func(t *testing.T, assets string) {
			if err := os.Mkdir(assets, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := mkfifoAsset(filepath.Join(assets, "stream")); err != nil {
				t.Skipf("FIFO unsupported: %v", err)
			}
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("kind: A\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			assets := filepath.Join(root, "gaggles", "example", "goobers", "coder", "assets")
			if err := os.MkdirAll(filepath.Dir(assets), 0o755); err != nil {
				t.Fatal(err)
			}
			setup(t, assets)
			if _, err := configDirectoryDigest(root); err == nil {
				t.Fatal("configDirectoryDigest accepted unsafe assets")
			}
		})
	}
}

func TestConfigDirectoryDigestSkipsVanishedFileWithoutError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("kind: A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A dangling symlink models a config path that WalkDir enumerates but that
	// has vanished by the time it is read (an atomic write-then-rename mid-poll).
	// The digest must skip it and succeed, never returning an error the poll
	// loop would journal as config.reload.rejected.
	if err := os.Symlink(filepath.Join(root, "gone.yaml"), filepath.Join(root, "pending.yaml")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := configDirectoryDigest(root); err != nil {
		t.Fatalf("vanished config file surfaced as error: %v", err)
	}
}
