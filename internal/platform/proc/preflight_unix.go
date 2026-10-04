//go:build unix

package proc

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const cleanupProbeTimeout = 5 * time.Second
const cleanupProbeSettle = time.Second

func probeCleanup(ctx context.Context) CleanupProbe {
	ctx, cancel := context.WithTimeout(ctx, cleanupProbeTimeout)
	defer cancel()
	// Both programs and the script are fixed, not instance/harness input. The
	// descendant has its own finite lifetime even if the supervisor fails.
	cmd := exec.Command("/bin/sh", "-c", `/bin/sleep 30 & child=$!; printf '%s\n' "$child"; wait`)
	cmd.Env = []string{}
	return runCleanupFixture(ctx, cmd)
}

func runCleanupFixture(ctx context.Context, cmd *exec.Cmd) CleanupProbe {
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return cleanupProbeFailure("cleanup_probe_start_failed")
	}
	tree, err := Start(cmd)
	if err != nil {
		_ = pipe.Close()
		return cleanupProbeFailure("cleanup_probe_start_failed")
	}
	child := readFixturePID(ctx, pipe)
	// Snapshot/kill before Wait: only this freshly started tree is signalled.
	killErr := tree.Kill()
	_ = pipe.Close()
	_ = cmd.Wait()
	if ctx.Err() != nil {
		return cleanupProbeFailure("cleanup_probe_canceled")
	}
	if child <= 0 || killErr != nil {
		return cleanupProbeFailure("cleanup_probe_failed")
	}
	if !awaitFixtureStopped(ctx, cmd.Process.Pid, child) {
		return cleanupProbeFailure("cleanup_probe_failed")
	}
	return CleanupProbe{"owned_cleanup_observed", "passed", "proc.Start/Tree.Kill terminated the controlled parent and same-group descendant; no surviving fixture processes observed"}
}

func readFixturePID(ctx context.Context, pipe io.Reader) int {
	ready := make(chan int, 1)
	go func() {
		line, _ := bufio.NewReader(io.LimitReader(pipe, 64)).ReadString('\n')
		pid, _ := strconv.Atoi(strings.TrimSpace(line))
		ready <- pid
	}()
	select {
	case pid := <-ready:
		return pid
	case <-ctx.Done():
		return 0
	}
}

func awaitFixtureStopped(ctx context.Context, pids ...int) bool {
	ctx, cancel := context.WithTimeout(ctx, cleanupProbeSettle)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		alive := false
		for _, pid := range pids {
			alive = alive || Alive(pid)
		}
		if !alive {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func cleanupProbeFailure(code string) CleanupProbe {
	return CleanupProbe{code, "unobservable", "controlled fixture cleanup was not proven; target cleanup remains unobservable"}
}
