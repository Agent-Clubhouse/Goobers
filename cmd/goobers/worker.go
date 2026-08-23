package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/internal/version"
	"github.com/goobers/goobers/internal/workerhost"
	"github.com/goobers/goobers/internal/worktree"
)

const workerHelp = "Usage: goobers worker [--task-queue <queue>]... [flags]\n\n" +
	"Host a Temporal worker for the tier-3 engine (experimental): connect to the\n" +
	"configured Temporal frontend, register the engine workflow and activities,\n" +
	"and serve the named task queue(s) until SIGTERM/SIGINT, then drain — stop\n" +
	"polling and let in-flight activities finish within --drain-timeout.\n\n" +
	"The tier-3 engine is not on the local (V0) execution path; this command is\n" +
	"the deployable worker shape for the cloud ladder. Automated gate checks and\n" +
	"workspace provisioning (git worktrees + scratch dirs under --work-root) are\n" +
	"wired. With --instance, the worker also wires the same agentic and\n" +
	"deterministic executors as the local runner; without it, stages needing\n" +
	"those executors fail closed with a clear error.\n\n" +
	"Flags:\n" +
	"  --instance <dir>           instance root; wires the real agentic and\n" +
	"                             deterministic executors (default\n" +
	"                             $GOOBERS_INSTANCE_ROOT)\n" +
	"  --blob-store <dir>         directory backing the fleet-wide\n" +
	"                             content-addressed artifact store; required\n" +
	"                             for a run whose stages are served by more\n" +
	"                             than one worker (default $GOOBERS_BLOB_STORE)\n" +
	"  --task-queue <queue>       task queue to serve; repeatable (default\n" +
	"                             engine.taskQueue, with env override)\n" +
	"  --temporal-hostport <h:p>  Temporal frontend (default engine.hostPort,\n" +
	"                             with env override)\n" +
	"  --temporal-namespace <ns>  Temporal namespace (default engine.namespace,\n" +
	"                             with env override)\n" +
	"  --drain-timeout <dur>      graceful-drain bound after a shutdown signal\n" +
	"                             (default 30s)\n" +
	"  --work-root <dir>          root for stage workspaces (default: a\n" +
	"                             goobers-worker dir under the OS temp dir)\n" +
	"  --daemon-api <url>         daemon write API base URL; wires live journal\n" +
	"                             emission through the journal plane, with the\n" +
	"                             per-run bearer from $GOOBERS_POD_TOKEN when\n" +
	"                             set (default $GOOBERS_DAEMON_API)\n" +
	"  --dispatch-namespace <ns>  namespace to create mode-3 stage pods in;\n" +
	"                             wires the dispatcher behind the stage-dispatch\n" +
	"                             seam and serves the per-(gaggle x runner)\n" +
	"                             dispatch queues derived from the instance's\n" +
	"                             runners: inventory. Requires --instance and\n" +
	"                             --blob-store (the surrender plane rides the\n" +
	"                             same volume); cluster access uses in-cluster\n" +
	"                             credentials or the standard kubeconfig rules\n" +
	"                             (default $GOOBERS_DISPATCH_NAMESPACE)\n\n" +
	"The worker identity reported to Temporal is versioned\n" +
	"(goobers-worker/<build>@<host>#<pid>) so visibility alone answers which\n" +
	"build serves a queue.\n\n" +
	"Exit codes: 0 = clean drain, 1 = startup/connection error, 2 = usage error,\n" +
	"3 = drain timeout expired with in-flight work abandoned.\n"

// workerAbandonedExit distinguishes a rollout that cut work short from a
// clean drain, so k8s/operators can alert on it.
const workerAbandonedExit = 3

// repeatableFlag collects a repeatable string flag in declaration order.
type repeatableFlag []string

func (f *repeatableFlag) String() string { return strings.Join(*f, ",") }

func (f *repeatableFlag) Set(v string) error {
	if v == "" {
		return errors.New("value must be non-empty")
	}
	*f = append(*f, v)
	return nil
}

func runWorker(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var queues repeatableFlag
	fs.Var(&queues, "task-queue", "task queue to serve (repeatable)")
	hostPort := fs.String("temporal-hostport", "", "Temporal frontend host:port")
	namespace := fs.String("temporal-namespace", "", "Temporal namespace")
	drain := fs.Duration("drain-timeout", workerhost.DefaultDrainTimeout, "graceful-drain bound after a shutdown signal")
	workRoot := fs.String("work-root", "", "root directory for stage workspaces")
	instanceRoot := fs.String("instance", workerEnvOr("GOOBERS_INSTANCE_ROOT", ""), "instance root; wires the real agentic and deterministic executors")
	blobRoot := fs.String("blob-store", workerEnvOr("GOOBERS_BLOB_STORE", ""), "directory backing the fleet-wide content-addressed artifact store")
	daemonAPI := fs.String("daemon-api", workerEnvOr("GOOBERS_DAEMON_API", ""), "daemon write API base URL for live journal emission")
	dispatchNamespace := fs.String("dispatch-namespace", workerEnvOr("GOOBERS_DISPATCH_NAMESPACE", ""), "namespace to create mode-3 stage pods in; enables the dispatcher-backed stage-dispatch seam")
	fs.Usage = helpUsage(stderr, "worker")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	engineConfig, err := resolveEngineConfig(*instanceRoot)
	if err != nil {
		pf(stderr, "error: load engine config: %v\n", err)
		return 2
	}
	if *hostPort == "" {
		*hostPort = engineConfig.HostPort
	}
	if *namespace == "" {
		*namespace = engineConfig.Namespace
	}
	if len(queues) == 0 {
		queues = repeatableFlag{engineConfig.TaskQueue}
	}

	root := *workRoot
	if root == "" {
		root = filepath.Join(os.TempDir(), "goobers-worker")
	}
	engineRuntime, err := workerEngineDeps(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	defer func() { _ = engineRuntime.Close() }()

	// The runtime wiring slice. Without --instance the worker keeps its
	// previous shape: workspaces and automated gates only, every real stage
	// failing closed with "not configured". With it, the agentic and
	// deterministic seams are the SAME executors the local runner builds, from
	// the same buildRunnerConfig — which is what journal conformance between
	// the two tiers rests on.
	if *instanceRoot != "" {
		// The fleet's content-addressed store, if one is configured. Without it
		// a run is only safely served by a SINGLE worker: stage artifacts stay
		// on the node that produced them, and the first ContextPointer resolved
		// somewhere else fails closed (#2866). It is constructed HERE, its only
		// consumer, so an instance-less worker with GOOBERS_BLOB_STORE set (a
		// fleet-wide env var) does not MkdirAll, emit a store line, or fail
		// closed on an unwritable path — the mode-1/2 self-only startup shape
		// stays byte-for-byte unchanged. The --dispatch-namespace path requires
		// --instance and reads *blobRoot directly (buildStageDispatch), never
		// this store value.
		var store blobstore.Store
		if *blobRoot != "" {
			dirStore, berr := blobstore.NewDir(*blobRoot)
			if berr != nil {
				pf(stderr, "error: %v\n", berr)
				return 1
			}
			store = dirStore
			pf(stdout, "goobers worker: artifact store %s\n", store.Describe())
		}
		seams, serr := newWorkerSeams(*instanceRoot, store)
		if serr != nil {
			pf(stderr, "error: %v\n", serr)
			return 1
		}
		engineRuntime.deps.Goober = seams.Agentic()
		engineRuntime.deps.Det = seams.Deterministic()
		// The #2931 dispatch canary asserts envelopes against the SAME shared
		// registry the seams' executors register every resolved credential
		// with — so a value that leaks into a dispatch payload after being
		// resolved anywhere in this process refuses the stage instead of
		// executing with it.
		engineRuntime.deps.Canary = seams.SharedRegistry()
		// Replace the uncredentialed provisioner too: workerEngineDeps builds
		// its worktree manager before any instance is known, so it has no git
		// auth and cannot clone a private repo.
		engineRuntime.deps.Workspaces = seams.Workspaces(filepath.Join(root, "scratch"))
		pf(stdout, "goobers worker: runtime seams wired from instance %s\n", *instanceRoot)
	}

	if *daemonAPI != "" {
		// The remote half of the DS4 emission seam: journal events for a
		// LiveJournal-pinned run flow to the daemon's journal plane. The
		// per-run pod bearer (internal/podauth) rides GOOBERS_POD_TOKEN;
		// empty is the loopback/no-auth posture.
		engineRuntime.deps.Journal = &livejournal.HTTPEmitter{
			BaseURL: *daemonAPI,
			Token:   workerEnvOr("GOOBERS_POD_TOKEN", ""),
		}
		pf(stdout, "goobers worker: live journal emission via %s\n", *daemonAPI)
	}

	if *dispatchNamespace != "" {
		// Mode-3 stage dispatch (#3588): wire the #3513 dispatcher behind the
		// engine's DispatchStage seam and serve the per-(gaggle ×
		// runner-type) dispatch queues beside the workflow queue(s). Requires
		// --instance: the runner inventory is what names the queues and the
		// eligible runners.
		if *instanceRoot == "" {
			pf(stderr, "error: --dispatch-namespace requires --instance (the runner inventory names the dispatch queues)\n")
			return 2
		}
		dispatch, derr := buildStageDispatch(*instanceRoot, *dispatchNamespace, *daemonAPI, *blobRoot)
		if derr != nil {
			pf(stderr, "error: %v\n", derr)
			return 1
		}
		engineRuntime.deps.Dispatcher = dispatch.Dispatcher
		engineRuntime.deps.Surrenders = dispatch.Surrenders
		queues = mergeQueues(queues, dispatch.Queues)
		pf(stdout, "goobers worker: mode-3 stage dispatch into namespace %s; dispatch queues %s\n",
			*dispatchNamespace, strings.Join(dispatch.Queues, ", "))
	}

	host, err := workerhost.New(workerhost.Config{
		HostPort:     *hostPort,
		Namespace:    *namespace,
		TaskQueues:   queues,
		DrainTimeout: *drain,
		BuildVersion: version.Get().Version,
		Deps:         engineRuntime.deps,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	ctx, stop := signals.SetupSignalContext()
	defer stop()
	pf(stdout, "goobers worker: serving task queue(s) %s on %s (namespace %s); identity %s\n",
		strings.Join(queues, ", "), *hostPort, *namespace, workerhost.Identity(version.Get().Version))
	err = host.Run(ctx)
	if errors.Is(err, workerhost.ErrAbandonedWork) {
		pf(stderr, "error: %v\n", err)
		return workerAbandonedExit
	}
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "goobers worker: drained cleanly\n")
	return 0
}

type workerEngineRuntime struct {
	deps      bootstrap.EngineDeps
	rootClaim *platformlock.Handle
}

func (r workerEngineRuntime) Close() error {
	return r.rootClaim.Release()
}

// workerEngineDeps wires the execution seams this slice of the worker
// provides: real workspace provisioning (worktrees + scratch dirs) and the
// pure automated gate evaluator. Agentic/deterministic executors belong to the
// runtime wiring slice; the engine's activities fail closed ("not configured")
// if a stage needs one.
func workerEngineDeps(workRoot string) (workerEngineRuntime, error) {
	owner, err := os.Hostname()
	if err != nil {
		return workerEngineRuntime{}, fmt.Errorf("resolve worker workspace owner: %w", err)
	}
	return workerEngineDepsForPlatform(workRoot, runtime.GOOS, owner)
}

const workerRootOwnerFile = ".goobers-worker-owner"

func workerEngineDepsForPlatform(workRoot, goos, owner string) (workerEngineRuntime, error) {
	rootClaim, err := claimWorkerRoot(workRoot, owner)
	if err != nil {
		return workerEngineRuntime{}, err
	}
	managerOptions := []worktree.ManagerOption{worktree.WithWriterIdentity(owner)}
	if goos == "windows" {
		managerOptions = append(managerOptions, worktree.WithDefaultPathLengthLimit(worktree.PathLengthLimit{}))
	}
	wtMgr, err := worktree.NewManager(filepath.Join(workRoot, "workcopies"), managerOptions...)
	if err != nil {
		return workerEngineRuntime{}, errors.Join(err, rootClaim.Release())
	}
	_, scrubber := journal.DefaultScrubber()
	return workerEngineRuntime{
		deps: bootstrap.EngineDeps{
			Auto: gate.NewAutomatedEvaluator(),
			Workspaces: &workerhost.WorktreeWorkspaces{
				Manager:    wtMgr,
				ScratchDir: filepath.Join(workRoot, "scratch"),
			},
			Scrubber: scrubber,
		},
		rootClaim: rootClaim,
	}, nil
}

func claimWorkerRoot(root, owner string) (*platformlock.Handle, error) {
	if owner == "" {
		return nil, errors.New("worker workspace owner must not be empty")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create worker workspace root %s: %w", root, err)
	}
	path := filepath.Join(root, workerRootOwnerFile)
	held, err := platformlock.TryAcquire(path)
	if errors.Is(err, platformlock.ErrHeld) {
		return nil, fmt.Errorf("worker workspace root %s is claimed by another live worker; each worker requires a private work root", root)
	}
	if err != nil {
		return nil, fmt.Errorf("claim worker workspace root %s: %w", root, err)
	}
	f := held.File()
	if err := f.Chmod(0o600); err != nil {
		_ = held.Release()
		return nil, fmt.Errorf("secure worker workspace owner %s: %w", path, err)
	}
	if err := f.Truncate(0); err != nil {
		_ = held.Release()
		return nil, fmt.Errorf("reset worker workspace owner %s: %w", path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = held.Release()
		return nil, fmt.Errorf("seek worker workspace owner %s: %w", path, err)
	}
	if _, err := fmt.Fprintln(f, owner); err != nil {
		_ = held.Release()
		return nil, fmt.Errorf("write worker workspace owner %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = held.Release()
		return nil, fmt.Errorf("sync worker workspace owner %s: %w", path, err)
	}
	return held, nil
}

func workerEnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
