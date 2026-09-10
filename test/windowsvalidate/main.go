//go:build windows

// Command windowsvalidate exercises a Windows node against the shipped binary
// and the canonical fake-harness implementation workflow. It writes the exact
// host/toolchain identity, workflow journal, test event stream, and daemon
// lifecycle record into an evidence directory.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

const (
	contractEvidenceDir       = "GOOBERS_SHIPPED_CONTRACT_EVIDENCE_DIR"
	daemonAPIAddressFileName  = "api.address"
	ephemeralAPIListenAddress = "127.0.0.1:0"
	// #4414 added preflight-repo-write as implementation.yaml's new first
	// stage (before query-backlog), which renumbers this scenario name —
	// contracts_test.go generates it from the first-encountered graph edge.
	implementationTestPattern = `^TestShippedWorkflowContracts$/^reference-workflows$/^goobers_implementation$/^01_preflight-repo-write_next$`
)

var (
	kernel32              = windows.NewLazySystemDLL("kernel32.dll")
	allocConsole          = kernel32.NewProc("AllocConsole")
	setConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
	ignoreConsoleCtrl     = syscall.NewCallback(func(uint32) uintptr { return 1 })
)

func main() {
	bin := flag.String("bin", filepath.Join("bin", "goobers.exe"), "path to the goobers binary to validate")
	outDir := flag.String("out", "windows-validation-evidence", "directory to write captured evidence into")
	flag.Parse()

	prepareConsole()

	if err := run(*bin, *outDir); err != nil {
		_ = os.MkdirAll(*outDir, 0o755)
		_ = os.WriteFile(filepath.Join(*outDir, "failure.txt"), []byte(err.Error()+"\n"), 0o644)
		fmt.Fprintf(os.Stderr, "windowsvalidate: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("windowsvalidate: PASS - daemon lifecycle + fake-harness implementation workflow validated")
}

func run(bin, outDir string) error {
	absBin, err := filepath.Abs(bin)
	if err != nil {
		return fmt.Errorf("resolve binary path: %w", err)
	}
	if _, err := os.Stat(absBin); err != nil {
		return fmt.Errorf("goobers binary not found at %s (build it first: `go build -o %s ./cmd/goobers`): %w", absBin, bin, err)
	}
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		return fmt.Errorf("resolve evidence directory: %w", err)
	}
	if err := os.MkdirAll(absOut, 0o755); err != nil {
		return fmt.Errorf("create evidence directory: %w", err)
	}

	var summary strings.Builder
	summary.WriteString("# Windows node validation evidence (#752)\n\n")

	environment := captureEnvironment(absBin)
	if err := os.WriteFile(filepath.Join(absOut, "environment.txt"), []byte(environment), 0o644); err != nil {
		return fmt.Errorf("write environment evidence: %w", err)
	}
	fmt.Fprintf(&summary, "## Validated environment\n\n```\n%s```\n\n", environment)

	workflowSummary, err := validateImplementationWorkflow(absOut)
	summary.WriteString(workflowSummary)
	if err != nil {
		return err
	}

	daemonSummary, err := validateDaemonLifecycle(absBin, absOut)
	summary.WriteString(daemonSummary)
	if err != nil {
		return err
	}

	summary.WriteString("\n## Breakage triage\n\nNo breakage was discovered; the triage list is empty.\n\n")
	summary.WriteString("**Result: PASS** - Windows is validated for deterministic workloads. Agentic parity is tracked separately by #647 Tier 2.\n")
	if err := os.WriteFile(filepath.Join(absOut, "summary.md"), []byte(summary.String()), 0o644); err != nil {
		return fmt.Errorf("write validation summary: %w", err)
	}
	return nil
}

func validateImplementationWorkflow(outDir string) (string, error) {
	root, err := repositoryRoot()
	if err != nil {
		return "", err
	}
	evidenceDir := filepath.Join(outDir, "implementation-workflow-journal")
	if err := os.RemoveAll(evidenceDir); err != nil {
		return "", fmt.Errorf("reset implementation-workflow evidence: %w", err)
	}

	command := exec.Command(
		"go", "test", "-json", "-count=1",
		"-run", implementationTestPattern,
		"./test/shippedworkflows",
	)
	command.Dir = root
	command.Env = append(os.Environ(), contractEvidenceDir+"="+evidenceDir)
	output, runErr := command.CombinedOutput()
	if err := os.WriteFile(filepath.Join(outDir, "implementation-workflow.json"), output, 0o644); err != nil {
		return "", fmt.Errorf("write implementation-workflow event stream: %w", err)
	}
	if runErr != nil {
		return "", fmt.Errorf("fake-harness implementation workflow failed: %w (see implementation-workflow.json)", runErr)
	}
	if err := validateImplementationJournal(evidenceDir); err != nil {
		return "", err
	}

	return "## Implementation workflow (real runner + deterministic fake harness)\n\n" +
		"The canonical shipped `reference-workflows/goobers/implementation` happy path reached " +
		"`phase=completed`. The captured journal records every task and gate from " +
		"`query-backlog` through `close-out`; `implementation-workflow.json` is the " +
		"structured test execution record.\n\n", nil
}

func validateImplementationJournal(runDir string) error {
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		return fmt.Errorf("open captured implementation journal: %w", err)
	}
	state, err := reader.State()
	if err != nil {
		return fmt.Errorf("read captured implementation state: %w", err)
	}
	if state.Phase != journal.PhaseCompleted {
		return fmt.Errorf("implementation workflow phase = %q, want %q", state.Phase, journal.PhaseCompleted)
	}
	events, err := reader.Events()
	if err != nil {
		return fmt.Errorf("read captured implementation events: %w", err)
	}
	var sequence []string
	for _, event := range events {
		switch event.Type {
		case journal.EventStageStarted:
			sequence = append(sequence, "stage:"+event.Stage)
		case journal.EventGateEvaluated:
			sequence = append(sequence, "gate:"+event.Gate+"="+event.Verdict)
		}
	}
	want := []string{
		"stage:preflight-repo-write",
		"stage:query-backlog",
		"stage:gather-implement-context",
		"stage:warm-module-cache",
		"gate:warm-module-cache-gate=pass",
		"stage:implement",
		"gate:review=pass",
		"stage:push-branch",
		"stage:local-ci",
		"gate:local-gate=pass",
		"stage:open-pr",
		"gate:open-pr-gate=pass",
		"stage:ci-poll",
		"gate:ci-gate=pass",
		"stage:close-out",
	}
	if strings.Join(sequence, "\n") != strings.Join(want, "\n") {
		return fmt.Errorf("implementation workflow sequence:\n got: %v\nwant: %v", sequence, want)
	}
	return nil
}

func validateDaemonLifecycle(bin, outDir string) (string, error) {
	instanceRoot := filepath.Join(outDir, "instance-daemon")
	if err := os.RemoveAll(instanceRoot); err != nil {
		return "", fmt.Errorf("reset daemon instance: %w", err)
	}
	if _, err := instance.InitDemo(instanceRoot); err != nil {
		return "", fmt.Errorf("scaffold daemon validation instance: %w", err)
	}
	if err := configureEphemeralAPI(instanceRoot); err != nil {
		return "", err
	}

	logPath := filepath.Join(outDir, "daemon.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return "", fmt.Errorf("create daemon log: %w", err)
	}
	cmd := exec.Command(bin, "up", "--quiet", instanceRoot)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// The daemon shares this process's console but leads its own process group,
	// so Ctrl+Break can be addressed to it alone. See sendCtrlBreak.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return "", fmt.Errorf("start `goobers up`: %w", err)
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	exited := false
	defer func() {
		if !exited {
			_ = cmd.Process.Kill()
			<-waitErr
		}
		_ = logFile.Close()
	}()

	readyAddress, processExited, err := waitForDaemonReadiness(instanceRoot, waitErr, 30*time.Second)
	if processExited {
		exited = true
	}
	if err != nil {
		return "", fmt.Errorf("%w\ndaemon log:\n%s", err, readLog(logFile, logPath))
	}

	status, statusErr := runGoobers(bin, 10*time.Second, "status", "--daemon", instanceRoot)
	if err := os.WriteFile(filepath.Join(outDir, "daemon-status.txt"), []byte(status), 0o644); err != nil {
		return "", fmt.Errorf("write daemon status evidence: %w", err)
	}
	if statusErr != nil {
		return "", fmt.Errorf("daemon API at %s reported ready but daemon status failed: %w\nstatus:\n%s\ndaemon log:\n%s",
			readyAddress, statusErr, status, readLog(logFile, logPath))
	}
	if !strings.Contains(status, "daemon running") {
		return "", fmt.Errorf("daemon API at %s reported ready but daemon status was not running:\n%s\ndaemon log:\n%s",
			readyAddress, status, readLog(logFile, logPath))
	}

	if err := sendCtrlBreak(uint32(cmd.Process.Pid)); err != nil {
		return "", fmt.Errorf("send Ctrl+Break to daemon process group: %w", err)
	}
	select {
	case err := <-waitErr:
		exited = true
		if err != nil {
			return "", fmt.Errorf("daemon did not exit cleanly after Ctrl+Break: %w\n%s", err, readLog(logFile, logPath))
		}
	case <-time.After(50 * time.Second):
		_ = cmd.Process.Kill()
		<-waitErr
		exited = true
		return "", fmt.Errorf("daemon did not exit within 50s after Ctrl+Break\n%s", readLog(logFile, logPath))
	}

	postStatus, postErr := runGoobers(bin, 10*time.Second, "status", "--daemon", instanceRoot)
	if err := os.WriteFile(filepath.Join(outDir, "daemon-post-stop.txt"), []byte(postStatus), 0o644); err != nil {
		return "", fmt.Errorf("write post-stop daemon status: %w", err)
	}
	if postErr == nil && strings.Contains(postStatus, "daemon running") {
		return "", fmt.Errorf("daemon still reports running after Ctrl+Break:\n%s", postStatus)
	}
	schedulerEvents, err := os.ReadFile(filepath.Join(instanceRoot, "scheduler", "events.jsonl"))
	if err != nil {
		return "", fmt.Errorf("read scheduler journal after daemon shutdown: %w", err)
	}
	if !strings.Contains(string(schedulerEvents), `"type":"daemon.clean_shutdown"`) {
		return "", fmt.Errorf("scheduler journal does not contain daemon.clean_shutdown")
	}

	return "## Foreground daemon lifecycle (real binary)\n\n" +
		"`goobers up` published its API, `/readyz` reported ready, " +
		"`status --daemon` reported it running, Ctrl+Break " +
		"triggered a clean exit, and the scheduler journal recorded " +
		"`daemon.clean_shutdown`.\n\n", nil
}

func waitForDaemonReadiness(instanceRoot string, waitErr <-chan error, timeout time.Duration) (string, bool, error) {
	addressPath := filepath.Join(instance.NewLayout(instanceRoot).SchedulerDir(), daemonAPIAddressFileName)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	var lastErr error
	for {
		data, err := os.ReadFile(addressPath)
		if err != nil {
			lastErr = fmt.Errorf("read daemon API address: %w", err)
		} else {
			address := strings.TrimSpace(string(data))
			if address == "" {
				lastErr = fmt.Errorf("daemon API address is empty")
			} else {
				response, requestErr := client.Get("http://" + address + httpapi.ReadinessPath)
				if requestErr != nil {
					lastErr = fmt.Errorf("query daemon readiness at %s: %w", address, requestErr)
				} else {
					var readiness httpapi.ReadinessStatus
					decodeErr := json.NewDecoder(response.Body).Decode(&readiness)
					closeErr := response.Body.Close()
					switch {
					case decodeErr != nil:
						lastErr = fmt.Errorf("decode daemon readiness at %s: %w", address, decodeErr)
					case closeErr != nil:
						lastErr = fmt.Errorf("close daemon readiness response at %s: %w", address, closeErr)
					case response.StatusCode == http.StatusOK && readiness.Ready:
						return address, false, nil
					default:
						lastErr = fmt.Errorf("daemon readiness at %s returned %s with ready=%t",
							address, response.Status, readiness.Ready)
					}
				}
			}
		}

		select {
		case err := <-waitErr:
			return "", true, fmt.Errorf("`goobers up` exited before its API became ready: %w", err)
		case <-deadline.C:
			return "", false, fmt.Errorf("daemon API did not become ready within %s: %w", timeout, lastErr)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// prepareConsole makes this process a safe sender of console control events.
//
// GenerateConsoleCtrlEvent requires the sender to share a console with the
// target group, and the Actions runner is a Windows service that may have none.
// AllocConsole supplies one; it fails with ERROR_ACCESS_DENIED when a console
// already exists, which is equally fine. It cannot disturb this process's
// output because Go binds os.Stdout/os.Stderr at startup, before any console of
// ours exists.
//
// The ignore handler is defence in depth. sendCtrlBreak addresses the daemon's
// own process group, so this process is never a recipient; the handler only
// ensures a stray event cannot terminate the validation run.
func prepareConsole() {
	_, _, _ = allocConsole.Call()
	_, _, _ = setConsoleCtrlHandler.Call(ignoreConsoleCtrl, 1)
}

// sendCtrlBreak asks the daemon's process group -- and only that group -- to
// shut down.
//
// The daemon is started with CREATE_NEW_PROCESS_GROUP, so its process ID is
// also its process group ID and Ctrl+Break can be addressed to it precisely.
// Passing group 0 instead would target every process sharing the console,
// including this one: console control events are delivered asynchronously, so
// a sender that targets itself races its own suppression and is killed with
// STATUS_CONTROL_C_EXIT (0xc000013a) roughly half the time.
func sendCtrlBreak(processGroupID uint32) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, processGroupID)
}

func configureEphemeralAPI(root string) error {
	path := filepath.Join(root, "instance.yaml")
	config, err := instance.LoadConfig(path)
	if err != nil {
		return fmt.Errorf("load daemon instance config: %w", err)
	}
	config.API.Listen = ephemeralAPIListenAddress
	if err := instance.WriteConfig(path, config); err != nil {
		return fmt.Errorf("write daemon instance config: %w", err)
	}
	return nil
}

func captureEnvironment(bin string) string {
	var output strings.Builder
	fmt.Fprintf(&output, "GOOS/GOARCH:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&output, "Windows:       %s\n", firstLine(commandOutput("cmd.exe", "/d", "/c", "ver")))
	fmt.Fprintf(&output, "runner image:  %s (version %s)\n", environmentValue("ImageOS"), environmentValue("ImageVersion"))
	fmt.Fprintf(&output, "Go toolchain:  %s\n", firstLine(commandOutput("go", "version")))
	fmt.Fprintf(&output, "git:           %s\n", firstLine(commandOutput("git", "--version")))
	fmt.Fprintf(&output, "goobers:       %s\n", firstLine(commandOutput(bin, "--version")))
	return output.String()
}

func repositoryRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve current directory: %w", err)
	}
	for {
		data, readErr := os.ReadFile(filepath.Join(dir, "go.mod"))
		if readErr == nil && strings.Contains(string(data), "module github.com/goobers/goobers") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("repository root not found from %s", dir)
		}
		dir = parent
	}
}

func runGoobers(bin string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return string(output), err
}

func commandOutput(name string, args ...string) string {
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("(unavailable: %v)", err)
	}
	return string(output)
}

func readLog(file *os.File, path string) string {
	_ = file.Sync()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(unavailable: %v)", err)
	}
	return string(data)
}

func environmentValue(name string) string {
	value := os.Getenv(name)
	if value == "" {
		return "local/unset"
	}
	return value
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return strings.TrimSpace(value[:index])
	}
	return value
}
