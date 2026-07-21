// Command linuxvalidate exercises the real `goobers` binary end to end on a
// POSIX host — the executable form of issue #636's "Linux node validation".
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
)

// runIDPattern matches the 32-hex-char run id printed by `goobers run` as
// "created run <id> (...)".
var runIDPattern = regexp.MustCompile(`created run ([0-9a-f]{32})`)

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
	name := "goobers"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join("bin", name)
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

	instance := filepath.Join(outDir, "instance")
	_ = os.RemoveAll(instance)

	// 2. Scaffold the credential-free deterministic demo instance.
	if out, err := runGoobers(absBin, 30*time.Second, "init", "--demo", instance); err != nil {
		return fmt.Errorf("init --demo failed: %w\n%s", err, out)
	}

	// 3. Drive the offline demo run to completion against the real binary.
	runOut, err := runGoobers(absBin, 2*time.Minute, "run", "demo", instance)
	if writeErr := os.WriteFile(filepath.Join(outDir, "demo-run.txt"), []byte(runOut), 0o644); writeErr != nil {
		return fmt.Errorf("write demo-run.txt: %w", writeErr)
	}
	if err != nil {
		return fmt.Errorf("`goobers run demo` did not complete cleanly (exit non-zero): %w\n%s", err, runOut)
	}
	if !strings.Contains(runOut, "finished: phase=completed") {
		return fmt.Errorf("`goobers run demo` did not reach phase=completed:\n%s", runOut)
	}
	m := runIDPattern.FindStringSubmatch(runOut)
	if m == nil {
		return fmt.Errorf("could not parse run id from `goobers run demo` output:\n%s", runOut)
	}
	runID := m[1]
	fmt.Fprintf(&summary, "## Offline demo run (real binary)\n\nRun `%s` reached `phase=completed`.\n\n", runID)

	// Capture the run journal as the execution record evidence.
	traceOut, traceErr := runGoobers(absBin, 30*time.Second, "trace", runID, instance)
	if traceErr != nil {
		return fmt.Errorf("`goobers trace %s` failed: %w\n%s", runID, traceErr, traceOut)
	}
	if err := os.WriteFile(filepath.Join(outDir, "demo-run-trace.txt"), []byte(traceOut), 0o644); err != nil {
		return fmt.Errorf("write demo-run-trace.txt: %w", err)
	}

	// 4. Daemon lifecycle: start → status --daemon → SIGTERM → clean shutdown.
	lifecycle, err := validateDaemonLifecycle(absBin, instance, outDir)
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
// SIGTERM, and confirms a clean shutdown that releases the lock. It returns a
// markdown fragment for the summary regardless of outcome.
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
	// Guarantee the child never leaks if we return early.
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		}
		_ = os.WriteFile(filepath.Join(outDir, "daemon.log"), daemonOut.Bytes(), 0o644)
	}()

	// Poll status --daemon until the daemon reports running (or time out).
	var lastStatus string
	running := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
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
	exitErr := waitWithTimeout(cmd, 50*time.Second)
	if exitErr != nil {
		return b.String(), fmt.Errorf("daemon did not exit cleanly after SIGTERM: %w\ndaemon log:\n%s", exitErr, daemonOut.String())
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

// waitWithTimeout waits for cmd to exit, requiring exit code 0, or returns an
// error if it exits non-zero or does not exit within d.
func waitWithTimeout(cmd *exec.Cmd, d time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("non-zero exit: %w", err)
		}
		return nil
	case <-time.After(d):
		_ = cmd.Process.Signal(syscall.SIGKILL)
		<-done
		return fmt.Errorf("timed out after %s waiting for graceful shutdown", d)
	}
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
