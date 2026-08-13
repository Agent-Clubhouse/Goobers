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

	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/gate"
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
	"wired; agentic and deterministic executor seams arrive with the runtime\n" +
	"wiring slice, and stages needing them fail closed with a clear error.\n\n" +
	"Flags:\n" +
	"  --task-queue <queue>       task queue to serve; repeatable for multiple\n" +
	"                             queues (default $GOOBERS_TASK_QUEUE, else\n" +
	"                             \"goobers-engine\")\n" +
	"  --temporal-hostport <h:p>  Temporal frontend (default\n" +
	"                             $GOOBERS_TEMPORAL_HOSTPORT, else 127.0.0.1:7233)\n" +
	"  --temporal-namespace <ns>  Temporal namespace (default\n" +
	"                             $GOOBERS_TEMPORAL_NAMESPACE, else \"default\")\n" +
	"  --drain-timeout <dur>      graceful-drain bound after a shutdown signal\n" +
	"                             (default 30s)\n" +
	"  --work-root <dir>          root for stage workspaces (default: a\n" +
	"                             goobers-worker dir under the OS temp dir)\n\n" +
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
	hostPort := fs.String("temporal-hostport", workerEnvOr("GOOBERS_TEMPORAL_HOSTPORT", "127.0.0.1:7233"), "Temporal frontend host:port")
	namespace := fs.String("temporal-namespace", workerEnvOr("GOOBERS_TEMPORAL_NAMESPACE", "default"), "Temporal namespace")
	drain := fs.Duration("drain-timeout", workerhost.DefaultDrainTimeout, "graceful-drain bound after a shutdown signal")
	workRoot := fs.String("work-root", "", "root directory for stage workspaces")
	fs.Usage = helpUsage(stderr, "worker")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	if len(queues) == 0 {
		queues = repeatableFlag{workerEnvOr("GOOBERS_TASK_QUEUE", bootstrap.DefaultTaskQueue)}
	}

	root := *workRoot
	if root == "" {
		root = filepath.Join(os.TempDir(), "goobers-worker")
	}
	deps, err := workerEngineDeps(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	host, err := workerhost.New(workerhost.Config{
		HostPort:     *hostPort,
		Namespace:    *namespace,
		TaskQueues:   queues,
		DrainTimeout: *drain,
		BuildVersion: version.Get().Version,
		Deps:         deps,
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

// workerEngineDeps wires the execution seams this slice of the worker
// provides: real workspace provisioning (worktrees + scratch dirs) and the
// pure automated gate evaluator. Agentic/deterministic executors belong to the
// runtime wiring slice; the engine's activities fail closed ("not configured")
// if a stage needs one.
func workerEngineDeps(workRoot string) (bootstrap.EngineDeps, error) {
	owner, err := os.Hostname()
	if err != nil {
		return bootstrap.EngineDeps{}, fmt.Errorf("resolve worker workspace owner: %w", err)
	}
	return workerEngineDepsForPlatform(workRoot, runtime.GOOS, owner)
}

const workerRootOwnerFile = ".goobers-worker-owner"

func workerEngineDepsForPlatform(workRoot, goos, owner string) (bootstrap.EngineDeps, error) {
	if err := claimWorkerRoot(workRoot, owner); err != nil {
		return bootstrap.EngineDeps{}, err
	}
	managerOptions := []worktree.ManagerOption{worktree.WithWriterIdentity(owner)}
	if goos == "windows" {
		managerOptions = append(managerOptions, worktree.WithDefaultPathLengthLimit(worktree.PathLengthLimit{}))
	}
	wtMgr, err := worktree.NewManager(filepath.Join(workRoot, "workcopies"), managerOptions...)
	if err != nil {
		return bootstrap.EngineDeps{}, err
	}
	return bootstrap.EngineDeps{
		Auto: gate.NewAutomatedEvaluator(),
		Workspaces: &workerhost.WorktreeWorkspaces{
			Manager:    wtMgr,
			ScratchDir: filepath.Join(workRoot, "scratch"),
		},
	}, nil
}

func claimWorkerRoot(root, owner string) error {
	if owner == "" {
		return errors.New("worker workspace owner must not be empty")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create worker workspace root %s: %w", root, err)
	}
	path := filepath.Join(root, workerRootOwnerFile)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if _, err := fmt.Fprintln(f, owner); err != nil {
			_ = f.Close()
			return fmt.Errorf("write worker workspace owner %s: %w", path, err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return fmt.Errorf("sync worker workspace owner %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close worker workspace owner %s: %w", path, err)
		}
		return nil
	}
	if !os.IsExist(err) {
		return fmt.Errorf("claim worker workspace root %s: %w", root, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read worker workspace owner %s: %w", path, err)
	}
	existing := strings.TrimSpace(string(data))
	if existing != owner {
		return fmt.Errorf("worker workspace root %s is owned by %q, not %q; each worker pod requires a private work root", root, existing, owner)
	}
	return nil
}

func workerEnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
