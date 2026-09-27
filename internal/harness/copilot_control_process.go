package harness

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/goobers/goobers/internal/journal"
)

// copilotControlExitDetailBytes bounds the stderr tail quoted when the control
// process exits before readiness (#5636).
const copilotControlExitDetailBytes = 4 << 10

// copilotControlProcess keeps the ordinary owned-process cleanup boundary while
// the SDK talks to its authenticated loopback endpoint. The SDK never launches
// a second process or inherits the daemon environment.
type copilotControlProcess struct {
	client *copilot.Client
	cancel context.CancelFunc
	done   <-chan struct{}
	result ProcessResult
	err    error
}

type copilotPortCapture struct {
	mu    sync.Mutex
	tail  string
	ready chan int
}

var copilotPortPattern = regexp.MustCompile(`listening on port (\d+)\r?\n`)

func (c *copilotPortCapture) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tail += string(data)
	if match := copilotPortPattern.FindStringSubmatch(c.tail); len(match) > 1 {
		port, err := strconv.Atoi(match[1])
		if err == nil && port > 0 && port <= 65535 {
			select {
			case c.ready <- port:
			default:
			}
		}
	}
	if len(c.tail) > 4096 {
		c.tail = c.tail[len(c.tail)-4096:]
	}
	return len(data), nil
}

func startCopilotControlProcess(ctx context.Context, runner ProcessRunner, req ProcessRequest, promptIndex int) (*copilotControlProcess, string, error) {
	command, sessionID, err := copilotControlCommand(req.Command, promptIndex)
	if err != nil {
		return nil, "", err
	}
	token, err := newHarnessSessionID()
	if err != nil {
		return nil, "", err
	}
	runCtx, cancel := context.WithCancel(ctx)
	capture := &copilotPortCapture{ready: make(chan int, 1)}
	done := make(chan struct{})
	process := &copilotControlProcess{cancel: cancel, done: done}
	req.Command = command
	req.Env = overrideEnv(req.Env, "COPILOT_CONNECTION_TOKEN", token)
	req.StdoutCapture = capture
	req.Activity = nil             // The session's RPC event stream owns activity observations.
	req.TranscriptCheckpoint = nil // RPC events, not server startup text, are model evidence.
	go func() { defer close(done); process.result, process.err = runner.Run(runCtx, req) }()
	startup, cancelStartup := context.WithTimeout(ctx, requiredMCPProbeTimeout)
	defer cancelStartup()
	var port int
	select {
	case port = <-capture.ready:
	case <-done:
		cancel()
		return nil, "", copilotControlExitError(process.result, process.err)
	case <-startup.Done():
		process.close()
		return nil, "", fmt.Errorf("%w: Copilot control startup timed out", errRequiredMCPUnavailable)
	}
	process.client = copilot.NewClient(&copilot.ClientOptions{Connection: copilot.URIConnection{URL: fmt.Sprintf("127.0.0.1:%d", port), ConnectionToken: token}, LogLevel: "error"})
	if err := process.client.Start(startup); err != nil {
		process.close()
		return nil, "", fmt.Errorf("%w: Copilot control connection failed", errRequiredMCPUnavailable)
	}
	return process, sessionID, nil
}

func (p *copilotControlProcess) close() {
	if p.client != nil {
		p.client.ForceStop()
	}
	p.cancel()
	<-p.done // ProcessRunner joins owned descendants before returning.
}

// copilotControlExitError names why the control process never announced its
// port. The CLI rejects a bad argv on stderr and exits within a second (#5636:
// --usage-output-file with --headless), so a bare "exited" reads the same as a
// crash; the exit status and a redacted stderr tail name the rejected option.
func copilotControlExitError(result ProcessResult, runErr error) error {
	status := fmt.Sprintf("exit %d", result.ExitCode)
	if result.ExitCode <= 0 && runErr != nil {
		status = runErr.Error()
	}
	detail := copilotControlStderrTail(result.Stderr)
	if detail != "" {
		detail = ": " + detail
	}
	return fmt.Errorf("%w: Copilot control process exited before readiness (%s)%s", errRequiredMCPUnavailable, status, detail)
}

// copilotControlStderrTail scrubs before clipping so a credential straddling
// the cut is never half-quoted, and keeps the tail: the CLI's last line is why
// it exited.
func copilotControlStderrTail(stderr []byte) string {
	detail := strings.TrimSpace(string(journal.NewPatternScrubber().Scrub(stderr)))
	if len(detail) <= copilotControlExitDetailBytes {
		return detail
	}
	cut := len(detail) - copilotControlExitDetailBytes
	for cut < len(detail) && !utf8.RuneStart(detail[cut]) {
		cut++
	}
	return "(truncated) …" + detail[cut:]
}

func copilotControlCommand(argv []string, promptIndex int) ([]string, string, error) {
	if promptIndex < 1 || promptIndex >= len(argv) {
		return nil, "", fmt.Errorf("invalid Copilot prompt position")
	}
	var command []string
	var session string
	for i, arg := range argv {
		if i == promptIndex {
			continue
		}
		if i > 0 && argv[i-1] == "--session-id" {
			session = arg
			continue
		}
		// Copilot CLI rejects --usage-output-file alongside --headless as a
		// usage error and exits before announcing its port (#5636). The
		// controlled path takes usage from the session's RPC events
		// (applyControlledCopilotUsage), and applyCopilotUsageDocument treats
		// the absent document as no data, so dropping the flag loses nothing.
		if i > 0 && argv[i-1] == "--usage-output-file" {
			continue
		}
		if arg == "--session-id" || arg == "--silent" || strings.HasPrefix(arg, "--output-format=") ||
			arg == "--usage-output-file" || strings.HasPrefix(arg, "--usage-output-file=") {
			continue
		}
		command = append(command, arg)
	}
	if session == "" {
		return nil, "", fmt.Errorf("copilot controlled session requires adapter-owned session identity")
	}
	return append(command, "--headless", "--no-auto-update", "--port", "0"), session, nil
}

// Retain a bounded call deadline even when a caller supplies an unbounded run.
func copilotSessionContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return context.WithTimeout(ctx, timeout)
}
