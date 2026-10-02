package main

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type child struct {
	cmd    *exec.Cmd
	done   chan struct{}
	log    *os.File
	cancel context.CancelFunc
}

func startChild(ctx context.Context, logPath, binary string, args ...string) (*child, error) {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	childCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(childCtx, binary, args...)
	cmd.Env = cleanEnv()
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		_ = log.Close()
		return nil, err
	}
	p := &child{cmd: cmd, done: make(chan struct{}), log: log, cancel: cancel}
	go func() { _ = cmd.Wait(); close(p.done) }()
	return p, nil
}

func (p *child) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *child) stop() {
	if p == nil {
		return
	}
	p.cancel()
	<-p.done // CommandContext's WaitDelay bounds interrupt handling.
	_ = p.log.Close()
}

// This guard is deliberately restrictive. #1480 owns Docker launch, disk
// throttling, OOM attribution, and the final outer verdict. A marker alone is
// insufficient: require finite cgroup-v2 CPU and memory ceilings too. No native
// host fallback, privilege escalation, or Docker Desktop execution is provided.
func requireIsolation(p Profile) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("pressure requires an isolated native-Linux Docker runtime (#1480), got %s", runtime.GOOS)
	}
	if _, err := os.Stat("/.dockerenv"); err != nil {
		return fmt.Errorf("pressure requires a Docker container: %w", err)
	}
	kernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return err
	}
	release := strings.ToLower(string(kernel))
	if strings.Contains(release, "linuxkit") || strings.Contains(release, "microsoft") {
		return fmt.Errorf("docker Desktop/WSL is not a native Linux soak environment")
	}
	cpu, err := os.ReadFile("/sys/fs/cgroup/cpu.max")
	if err != nil {
		return err
	}
	memory, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return err
	}
	return checkLimits(p, string(cpu), string(memory))
}

func checkLimits(p Profile, cpu, memory string) error {
	parts := strings.Fields(cpu)
	if len(parts) != 2 {
		return fmt.Errorf("invalid cpu.max")
	}
	quota, qerr := strconv.ParseFloat(parts[0], 64)
	period, perr := strconv.ParseFloat(parts[1], 64)
	mem, merr := strconv.ParseInt(strings.TrimSpace(memory), 10, 64)
	if qerr != nil || perr != nil || merr != nil || quota <= 0 || period <= 0 || mem <= 0 {
		return fmt.Errorf("finite CPU and memory cgroup limits are required")
	}
	if math.Abs(quota/period-p.CPULimit) > 0.001 || mem != int64(p.MemoryLimitMB)*1024*1024 {
		return fmt.Errorf("cgroup limits do not match profile %s", p.Name)
	}
	return nil
}

func startBackend(ctx context.Context, binary, root string, p Profile) (*cliBackend, invalidReason, error) {
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := initialize(initCtx, binary, root, p); err != nil {
		return nil, daemonHealthFailed, err
	}
	b := &cliBackend{binary: binary, root: root, client: &http.Client{Timeout: 5 * time.Second}}
	decisions, err := os.OpenFile(filepath.Join(root, "admissions.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, observationLost, err
	}
	b.decisions = decisions
	daemon, err := startChild(ctx, filepath.Join(root, "daemon.log"), binary, "up", "--quiet", root)
	if err != nil {
		_ = decisions.Close()
		return nil, daemonHealthFailed, err
	}
	b.daemon = daemon
	if err := b.awaitDaemon(ctx); err != nil {
		daemon.stop()
		_ = decisions.Close()
		return nil, daemonHealthFailed, err
	}
	pressure, err := startChild(ctx, filepath.Join(root, "pressure.log"), "stress-ng",
		"--cpu", strconv.Itoa(int(math.Ceil(p.CPULimit))), "--vm", "1", "--vm-bytes", strconv.Itoa(p.MemoryLimitMB/4)+"M",
		"--hdd", "1", "--hdd-bytes", "32M", "--temp-path", root, "--metrics-brief")
	if err != nil {
		daemon.stop()
		_ = decisions.Close()
		return nil, loadInjectorCrashed, err
	}
	b.pressure = pressure
	return b, "", nil
}

func (b *cliBackend) awaitDaemon(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		if b.daemon.exited() {
			return fmt.Errorf("daemon exited during startup; see daemon.log")
		}
		data, err := os.ReadFile(filepath.Join(b.root, "scheduler", "api.address"))
		if err == nil {
			address := strings.TrimSpace(string(data))
			host, _, err := net.SplitHostPort(address)
			if err != nil || host != "127.0.0.1" {
				return fmt.Errorf("unexpected daemon API address %q", address)
			}
			b.endpoint = "http://" + address
			if _, err := b.List(ctx, listOptions(time.Now())); err == nil {
				return nil
			}
		}
		if err := (wallClock{}).Wait(ctx, 100*time.Millisecond); err != nil {
			return fmt.Errorf("daemon health: %w", err)
		}
	}
}
