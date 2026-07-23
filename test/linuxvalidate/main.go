//go:build unix

// Command linuxvalidate exercises the real `goobers` binary end to end on a
// POSIX host — the executable form of issue #636's "Linux node validation".
// It is a POSIX-only orchestrator (it sets syscall.SysProcAttr.Setpgid to run
// the binary in its own process group), so it is constrained to unix to keep
// `GOOS=windows go build ./...` green — it never needs to compile on Windows.
//
// It proves, against the shipped binary rather than in-process test seams:
//
//  1. a full implementation-workflow shape runs to completion offline — it
//     scaffolds the credential-free `--demo` instance and drives `goobers run
//     demo` (deterministic triage → build → verdict, network:none) to a
//     `completed` phase, capturing the run journal as evidence; and
//  2. the daemon lifecycle works — it starts `goobers up`, confirms `goobers
//     status --daemon` reports the daemon running, sends SIGTERM, and confirms a
//     clean (exit 0) graceful shutdown that releases the instance lock.
//
// It also records the exact validated environment (OS/arch, kernel, distro, Go
// and git versions) so "supported on Linux" has a concrete, per-run referent.
//
// The demo run and the daemon lifecycle use SEPARATE instance roots on purpose:
// `goobers run` takes the same `scheduler/up.lock` single-instance lock that
// `goobers up` does (cmd/goobers/run.go), so sharing one root lets the
// subsequent `up` race the just-finished run's lock release under CI load. Two
// roots remove that shared lock entirely.
//
// Everything is offline and deterministic, so it is safe to run as a required
// CI job on ubuntu (see .github/workflows/ci.yml) and locally on macOS. It is a
// portable Go program with no shell dependency, matching the repository's
// Go-runner-first toolchain (design P5). Exit code 0 means every check passed;
// any failure exits non-zero with a diagnostic and leaves the evidence dir
// populated for inspection.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

// runIDPattern matches the 32-hex-char run id printed by `goobers run` as
// "created run <id> (...)".
var runIDPattern = regexp.MustCompile(`created run ([0-9a-f]{32})`)

const ephemeralAPIListenAddress = "127.0.0.1:0"

func main() {
	bin := flag.String("bin", defaultBin(), "path to the goobers binary to validate")
	outDir := flag.String("out", "linux-validation-evidence", "directory to write captured evidence into")
	flag.Parse()

	if err := run(*bin, *outDir); err != nil {
		fmt.Fprintf(os.Stderr, "linuxvalidate: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("linuxvalidate: PASS — daemon lifecycle + offline demo run validated")
}

func defaultBin() string {
	// This orchestrator is unix-only (//go:build unix), so the built binary is
	// always plain "bin/goobers" — no ".exe" branch, which staticcheck would
	// (correctly) flag as unreachable dead code under the build constraint.
	return filepath.Join("bin", "goobers")
}

func run(bin, outDir string) error {
	absBin, err := filepath.Abs(bin)
	if err != nil {
		return fmt.Errorf("resolve binary path: %w", err)
	}
	if _, err := os.Stat(absBin); err != nil {
		return fmt.Errorf("goobers binary not found at %s (build it first: `go build -o %s ./cmd/goobers`): %w", absBin, bin, err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create evidence dir: %w", err)
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "# Linux node validation evidence (#636)\n\n")

	// 1. Record the validated environment precisely.
	env := captureEnvironment(absBin)
	if err := os.WriteFile(filepath.Join(outDir, "environment.txt"), []byte(env), 0o644); err != nil {
		return fmt.Errorf("write environment.txt: %w", err)
	}
	fmt.Fprintf(&summary, "## Validated environment\n\n```\n%s```\n\n", env)

	// 2 + 3. Hermetic full-loop demo run to completion (its own instance root).
	runInstance := filepath.Join(outDir, "instance-run")
	_ = os.RemoveAll(runInstance)
	demoSummary, err := validateDemoRun(absBin, runInstance, outDir)
	summary.WriteString(demoSummary)
	if err != nil {
		return err
	}

	// 4. Daemon lifecycle on a SEPARATE instance root (no shared up.lock).
	daemonInstance := filepath.Join(outDir, "instance-daemon")
	_ = os.RemoveAll(daemonInstance)
	if out, err := runGoobers(absBin, 30*time.Second, "init", "--demo", daemonInstance); err != nil {
		return fmt.Errorf("init --demo (daemon instance) failed: %w\n%s", err, out)
	}
	if err := configureEphemeralAPI(daemonInstance); err != nil {
		return err
	}
	lifecycle, err := validateDaemonLifecycle(absBin, daemonInstance, outDir)
	summary.WriteString(lifecycle)
	if err != nil {
		return err
	}

	summary.WriteString("\n**Result: PASS** — all Linux node validation checks passed.\n")
	if err := os.WriteFile(filepath.Join(outDir, "summary.md"), []byte(summary.String()), 0o644); err != nil {
		return fmt.Errorf("write summary.md: %w", err)
	}
	return nil
}

func configureEphemeralAPI(root string) error {
	path := filepath.Join(root, "instance.yaml")
	cfg, err := instance.LoadConfig(path)
	if err != nil {
		return fmt.Errorf("load daemon instance config: %w", err)
	}
	cfg.API.Listen = ephemeralAPIListenAddress
	if err := instance.WriteConfig(path, cfg); err != nil {
		return fmt.Errorf("write daemon instance config: %w", err)
	}
	return nil
}

// validateDemoRun scaffolds the hermetic demo instance, drives its mock-provider
// full loop to phase=completed against the real binary, and captures the journal.
func validateDemoRun(bin, instance, outDir string) (string, error) {
	var b strings.Builder
	if out, err := runGoobers(bin, 30*time.Second, "init", "--demo", instance); err != nil {
		return b.String(), fmt.Errorf("init --demo (run instance) failed: %w\n%s", err, out)
	}

	runOut, err := runGoobers(bin, 2*time.Minute, "run", "demo", instance)
	if writeErr := os.WriteFile(filepath.Join(outDir, "demo-run.txt"), []byte(runOut), 0o644); writeErr != nil {
		return b.String(), fmt.Errorf("write demo-run.txt: %w", writeErr)
	}
	if err != nil {
		return b.String(), fmt.Errorf("`goobers run demo` did not complete cleanly (exit non-zero): %w\n%s", err, runOut)
	}
	if !strings.Contains(runOut, "finished: phase=completed") {
		return b.String(), fmt.Errorf("`goobers run demo` did not reach phase=completed:\n%s", runOut)
	}
	m := runIDPattern.FindStringSubmatch(runOut)
	if m == nil {
		return b.String(), fmt.Errorf("could not parse run id from `goobers run demo` output:\n%s", runOut)
	}
	runID := m[1]
	fmt.Fprintf(&b, "## Offline demo run (real binary)\n\nRun `%s` reached `phase=completed`.\n\n", runID)

	traceOut, traceErr := runGoobers(bin, 30*time.Second, "trace", runID, instance)
	if traceErr != nil {
		return b.String(), fmt.Errorf("`goobers trace %s` failed: %w\n%s", runID, traceErr, traceOut)
	}
	for _, phase := range []string{"curate", "implement", "review", "merge-preview"} {
		if !strings.Contains(traceOut, "stage="+phase) {
			return b.String(), fmt.Errorf("demo trace did not reach %s:\n%s", phase, traceOut)
		}
	}
	if !strings.Contains(traceOut, `"provider":"mock"`) ||
		!strings.Contains(traceOut, `"mergePreview":"would squash mock pull request #1 into main"`) {
		return b.String(), fmt.Errorf("demo trace did not record the mock-provider merge preview:\n%s", traceOut)
	}
	if err := os.WriteFile(filepath.Join(outDir, "demo-run-trace.txt"), []byte(traceOut), 0o644); err != nil {
		return b.String(), fmt.Errorf("write demo-run-trace.txt: %w", err)
	}
	return b.String(), nil
}

// captureEnvironment records the concrete platform "supported" refers to.
func captureEnvironment(bin string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GOOS/GOARCH:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "Go runtime:    %s\n", runtime.Version())
	fmt.Fprintf(&b, "kernel:        %s\n", firstLine(commandOutput("uname", "-a")))
	fmt.Fprintf(&b, "distro:        %s\n", osRelease())
	fmt.Fprintf(&b, "git:           %s\n", firstLine(commandOutput("git", "--version")))
	fmt.Fprintf(&b, "goobers:       %s\n", firstLine(commandOutput(bin, "--version")))
	return b.String()
}

func osRelease() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "(no /etc/os-release; likely non-Linux host)"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
	}
	return "(PRETTY_NAME not found)"
}

// validateDaemonLifecycle starts the daemon, confirms status, stops it with
// SIGTERM, and confirms a clean shutdown that releases the lock. It fails fast
// (with the daemon log) if `up` exits before ever reporting running, rather than
// waiting out the full poll deadline. It returns a markdown fragment for the
// summary regardless of outcome.
func validateDaemonLifecycle(bin, instance, outDir string) (string, error) {
	var b strings.Builder
	b.WriteString("## Daemon lifecycle (real binary + real SIGTERM)\n\n")

	var daemonOut bytes.Buffer
	cmd := exec.Command(bin, "up", "--quiet", instance)
	cmd.Stdout = &daemonOut
	cmd.Stderr = &daemonOut
	// Own process group so a wedged child can be cleaned up deterministically.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return b.String(), fmt.Errorf("start `goobers up`: %w", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	exited := false
	defer func() {
		if !exited {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			<-waitErr
		}
		_ = os.WriteFile(filepath.Join(outDir, "daemon.log"), daemonOut.Bytes(), 0o644)
	}()

	// Poll status --daemon until the daemon reports running, failing fast if the
	// daemon process exits first (e.g. failed to acquire the lock).
	var lastStatus string
	running := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-waitErr:
			exited = true
			return b.String(), fmt.Errorf("`goobers up` exited before reporting running: %w\ndaemon log:\n%s", err, daemonOut.String())
		default:
		}
		out, err := runGoobers(bin, 10*time.Second, "status", "--daemon", instance)
		lastStatus = out
		if err == nil && strings.Contains(out, "daemon running") {
			running = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	_ = os.WriteFile(filepath.Join(outDir, "daemon-status.txt"), []byte(lastStatus), 0o644)
	if !running {
		return b.String(), fmt.Errorf("daemon did not report running within 30s; last status:\n%s\ndaemon log:\n%s", lastStatus, daemonOut.String())
	}
	fmt.Fprintf(&b, "- `up` started; `status --daemon` reported: `%s`\n", firstLine(lastStatus))

	// Stop with SIGTERM and require a clean, graceful shutdown.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return b.String(), fmt.Errorf("send SIGTERM to daemon: %w", err)
	}
	select {
	case err := <-waitErr:
		exited = true
		if err != nil {
			return b.String(), fmt.Errorf("daemon did not exit cleanly after SIGTERM: %w\ndaemon log:\n%s", err, daemonOut.String())
		}
	case <-time.After(50 * time.Second):
		_ = cmd.Process.Signal(syscall.SIGKILL)
		<-waitErr
		exited = true
		return b.String(), fmt.Errorf("daemon did not exit within 50s after SIGTERM\ndaemon log:\n%s", daemonOut.String())
	}
	b.WriteString("- SIGTERM ⇒ graceful shutdown, exit code 0.\n")

	// Confirm the lock was released: status --daemon now reports not-running.
	postOut, postErr := runGoobers(bin, 10*time.Second, "status", "--daemon", instance)
	if postErr == nil && strings.Contains(postOut, "daemon running") {
		return b.String(), fmt.Errorf("daemon still reports running after shutdown; lock not released:\n%s", postOut)
	}
	b.WriteString("- Post-stop `status --daemon` reports not running (lock released).\n")
	return b.String(), nil
}

// runGoobers runs the binary with args under a timeout, returning combined
// output. A non-zero exit is returned as an error (with the output preserved).
func runGoobers(bin string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func commandOutput(name string, args ...string) string {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("(unavailable: %v)", err)
	}
	return string(out)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
