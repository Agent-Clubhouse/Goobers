package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	hashiversion "github.com/hashicorp/go-version"

	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/proc"
	"github.com/goobers/goobers/internal/version"
)

const defaultDrainTimeout = 40 * time.Second

type escalator interface {
	Escalate(context.Context, Request, string) error
}
type process interface {
	Done() <-chan error
	Kill() error
}
type launcher interface {
	Start(string, string, io.Writer, io.Writer) (process, error)
}

// SupervisorOptions configures the stable supervisor process.
type SupervisorOptions struct {
	Root           string
	GOOS           string
	Launcher       launcher
	Escalator      escalator
	Stdout, Stderr io.Writer
	PollInterval   time.Duration
	DrainTimeout   time.Duration
	runner         commandRunner
	executable     string
	supervisor     versionInfo
}
type execLauncher struct{}
type execProcess struct {
	tree *proc.Tree
	done chan error
}

func (execLauncher) Start(binary, root string, stdout, stderr io.Writer) (process, error) {
	command := exec.Command(binary, "up", root)
	command.Stdout, command.Stderr, command.Env = stdout, stderr, os.Environ()
	tree, err := proc.Start(command)
	if err != nil {
		return nil, err
	}
	process := &execProcess{tree: tree, done: make(chan error, 1)}
	go func() {
		process.done <- command.Wait()
		close(process.done)
	}()
	return process, nil
}
func (p *execProcess) Done() <-chan error { return p.done }
func (p *execProcess) Kill() error        { return p.tree.Kill() }

// RunSupervisor owns the mutable daemon binary and its update state machine.
func RunSupervisor(ctx context.Context, opts SupervisorOptions) (retErr error) {
	opts = defaultSupervisorOptions(opts)
	if opts.Root == "" {
		return errors.New("supervisor instance root is required")
	}
	if err := ensureCurrentBinary(opts); err != nil {
		return err
	}
	log, _, err := journal.OpenInstanceLog(filepath.Join(opts.Root, "scheduler"))
	if err != nil {
		return fmt.Errorf("open supervisor journal: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, log.Close()) }()

	request, pending, err := pendingRequest(opts.Root)
	if err != nil {
		if err := rejectInvalidRequest(opts, err); err != nil {
			return err
		}
		pending = false
	}
	if pending {
		pending, err = recoverRequest(opts, log, &request)
		if err != nil {
			return err
		}
	}
	process, err := opts.Launcher.Start(currentBinary(opts.Root, opts.GOOS), opts.Root, opts.Stdout, opts.Stderr)
	if err != nil {
		return fmt.Errorf("start supervised daemon: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, terminateProcess(process, opts.DrainTimeout)) }()
	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()
	for {
		if pending {
			if request.Status != "rollback" {
				process, err = performUpdate(ctx, opts, log, process, &request)
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return stopForService(process, opts)
					}
					return err
				}
			}
			if request.Status == "rollback" {
				if err := escalateRollback(ctx, opts, log, request); err != nil {
					_, _ = fmt.Fprintf(opts.Stderr, "self-update rollback escalation failed; will retry: %v\n", err)
				}
			}
			pending = false
		}
		select {
		case <-ctx.Done():
			return stopForService(process, opts)
		case childErr := <-process.Done():
			if childErr == nil {
				return errors.New("supervised daemon exited unexpectedly")
			}
			return fmt.Errorf("supervised daemon exited: %w", childErr)
		case <-ticker.C:
			request, pending, err = pendingRequest(opts.Root)
			if err != nil {
				if err := rejectInvalidRequest(opts, err); err != nil {
					return err
				}
				pending = false
			}
		}
	}
}

func performUpdate(
	ctx context.Context,
	opts SupervisorOptions,
	log *journal.InstanceLog,
	process process,
	request *Request,
) (process, error) {
	if request.Status == "requested" {
		if err := ensureUpdateEvent(opts.Root, log, journal.EventDaemonUpdateDrainStarted, *request, "draining before binary handoff"); err != nil {
			return process, err
		}
		if err := setStatus(opts.Root, request, "draining"); err != nil {
			return process, err
		}
	}
	if err := RequestDaemonStop(opts.Root); err != nil {
		return process, err
	}
	if err := waitOrKill(process, opts.DrainTimeout); err != nil {
		return process, fmt.Errorf("daemon failed while draining for self-update: %w", err)
	}
	_ = os.Remove(stopRequestPath(opts.Root))

	candidate, baseline, err := startCandidate(opts, log, request)
	if err != nil {
		if request.Status != "monitoring" {
			return candidate, err
		}
		return rollbackAndRestart(opts, log, request, candidate, err.Error())
	}
	alive, reason, err := monitorCandidate(ctx, opts, log, candidate, baseline, request)
	if err != nil {
		return candidate, err
	}
	if reason == "" {
		return candidate, nil
	}
	if !alive {
		candidate = nil
	}
	return rollbackAndRestart(opts, log, request, candidate, reason)
}

func monitorCandidate(
	ctx context.Context,
	opts SupervisorOptions,
	log *journal.InstanceLog,
	process process,
	lastHeartbeat time.Time,
	request *Request,
) (bool, string, error) {
	timeout, _ := time.ParseDuration(request.HealthTimeout)
	started, baselineSet, cleanTicks := time.Now(), false, 0
	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return true, "", ctx.Err()
		case processErr := <-process.Done():
			reason := "candidate daemon exited before completing its health window"
			if processErr != nil {
				reason += ": " + processErr.Error()
			}
			return false, reason, nil
		case <-ticker.C:
			if time.Since(started) > timeout {
				return true, "candidate did not complete its bounded health window", nil
			}
			heartbeat, err := readHeartbeat(opts.Root)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return true, "read candidate heartbeat: " + err.Error(), nil
			}
			if !heartbeat.After(lastHeartbeat) {
				continue
			}
			lastHeartbeat = heartbeat
			if baselineSet {
				cleanTicks++
			} else {
				baselineSet = true
			}
			if cleanTicks < request.HealthTicks {
				continue
			}
			if err := setStatus(opts.Root, request, "healthy"); err != nil {
				return true, "", err
			}
			return true, "", finalizeHealthyUpdate(opts.Root, log, *request)
		}
	}
}

func startCandidate(opts SupervisorOptions, log *journal.InstanceLog, request *Request) (process, time.Time, error) {
	if err := setStatus(opts.Root, request, "activating"); err != nil {
		return nil, time.Time{}, err
	}
	if err := activateCandidate(opts, request); err != nil {
		return nil, time.Time{}, fmt.Errorf("activate staged binary: %w", err)
	}
	if err := setStatus(opts.Root, request, "monitoring"); err != nil {
		return nil, time.Time{}, err
	}
	baseline, err := readHeartbeat(opts.Root)
	if errors.Is(err, os.ErrNotExist) {
		baseline, err = time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read candidate heartbeat baseline: %w", err)
	}
	process, err := opts.Launcher.Start(currentBinary(opts.Root, opts.GOOS), opts.Root, opts.Stdout, opts.Stderr)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("candidate binary failed to start: %w", err)
	}
	if err := appendUpdateEvent(log, journal.EventDaemonUpdateRestarted, *request, "candidate binary started"); err != nil {
		return process, time.Time{}, err
	}
	return process, baseline, nil
}

func recoverRequest(opts SupervisorOptions, log *journal.InstanceLog, request *Request) (bool, error) {
	switch request.Status {
	case "healthy":
		return false, finalizeHealthyUpdate(opts.Root, log, *request)
	case "activating":
		if request.RollbackReady {
			if err := restorePrevious(opts); err != nil {
				return true, err
			}
		}
		return true, markRollback(opts.Root, log, request, "stable supervisor restarted during candidate activation")
	case "monitoring":
		if err := restorePrevious(opts); err != nil {
			return true, err
		}
		return true, markRollback(opts.Root, log, request, "stable supervisor restarted before the candidate completed its health window")
	case "rollback":
		if err := restorePrevious(opts); err != nil {
			return true, err
		}
		return true, ensureUpdateEvent(opts.Root, log, journal.EventDaemonUpdateRolledBack, *request, request.Reason)
	default:
		return true, nil
	}
}

func rollbackAndRestart(
	opts SupervisorOptions,
	log *journal.InstanceLog,
	request *Request,
	process process,
	reason string,
) (process, error) {
	if process != nil {
		if err := RequestDaemonStop(opts.Root); err != nil {
			return nil, err
		}
		if err := waitOrKill(process, opts.DrainTimeout); err != nil {
			return nil, err
		}
		_ = os.Remove(stopRequestPath(opts.Root))
	}
	if err := restorePrevious(opts); err != nil {
		return nil, err
	}
	if err := markRollback(opts.Root, log, request, reason); err != nil {
		return nil, err
	}
	restored, err := opts.Launcher.Start(currentBinary(opts.Root, opts.GOOS), opts.Root, opts.Stdout, opts.Stderr)
	if err != nil {
		return nil, fmt.Errorf("restart retained previous binary: %w", err)
	}
	return restored, nil
}

func markRollback(root string, log *journal.InstanceLog, request *Request, reason string) error {
	request.Reason = reason
	if err := setStatus(root, request, "rollback"); err != nil {
		return err
	}
	return ensureUpdateEvent(root, log, journal.EventDaemonUpdateRolledBack, *request, reason)
}

func setStatus(root string, request *Request, status string) error {
	request.Status = status
	return writeRequest(root, *request)
}

func escalateRollback(ctx context.Context, opts SupervisorOptions, log *journal.InstanceLog, request Request) error {
	if opts.Escalator == nil {
		return errors.New("self-update rollback requires an escalation provider")
	}
	if err := opts.Escalator.Escalate(ctx, request, request.Reason); err != nil {
		return fmt.Errorf("create self-update rollback escalation: %w", err)
	}
	if err := ensureUpdateEvent(opts.Root, log, journal.EventDaemonUpdateEscalated, request, "rollback escalation issue created"); err != nil {
		return err
	}
	return completeRequest(opts.Root, request)
}

func finalizeHealthyUpdate(root string, log *journal.InstanceLog, request Request) error {
	if err := ensureUpdateEvent(root, log, journal.EventDaemonUpdateHealthy, request, "candidate completed clean heartbeat window"); err != nil {
		return err
	}
	return completeRequest(root, request)
}

func completeRequest(root string, request Request) error {
	if err := removeCompletedStaging(root, request.StagedPath); err != nil {
		return fmt.Errorf("clean self-update staging: %w", err)
	}
	if err := os.Remove(requestPath(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("complete self-update request: %w", err)
	}
	return nil
}

func defaultSupervisorOptions(opts SupervisorOptions) SupervisorOptions {
	opts.GOOS = valueOr(opts.GOOS, runtime.GOOS)
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPollInterval
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = defaultDrainTimeout
	}
	if opts.Launcher == nil {
		opts.Launcher = execLauncher{}
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.runner == nil {
		opts.runner = execRunner{}
	}
	if opts.supervisor == (versionInfo{}) {
		info := version.Get()
		opts.supervisor = versionInfo{Version: info.Version, Commit: info.Commit}
	}
	return opts
}

func ensureCurrentBinary(opts SupervisorOptions) error {
	current := currentBinary(opts.Root, opts.GOOS)
	executable := opts.executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return fmt.Errorf("resolve supervisor executable: %w", err)
		}
	}
	if _, err := os.Stat(current); errors.Is(err, os.ErrNotExist) {
		return copyExecutable(executable, current)
	} else if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active, err := readVersion(ctx, opts.runner, opts.Root, current)
	if err != nil {
		_, reportErr := fmt.Fprintf(opts.Stderr,
			"self-update skew: cannot read supervised daemon version at %s: %v; keeping existing binary\n",
			current, err)
		return reportErr
	}
	if sameVersionInfo(opts.supervisor, active) {
		return nil
	}
	installedVersion, installedErr := hashiversion.NewVersion(opts.supervisor.Version)
	activeVersion, activeErr := hashiversion.NewVersion(active.Version)
	if installedErr != nil || activeErr != nil {
		_, reportErr := fmt.Fprintf(opts.Stderr,
			"self-update skew: installed supervisor is %s, supervised daemon is %s; versions are not orderable, keeping existing binary\n",
			formatVersionInfo(opts.supervisor), formatVersionInfo(active))
		return reportErr
	}
	if installedVersion.GreaterThan(activeVersion) {
		if err := copyExecutable(executable, current); err != nil {
			return fmt.Errorf("refresh supervised daemon from installed %s: %w", formatVersionInfo(opts.supervisor), err)
		}
		_, reportErr := fmt.Fprintf(opts.Stderr,
			"self-update: refreshed supervised daemon from %s to installed %s\n",
			formatVersionInfo(active), formatVersionInfo(opts.supervisor))
		return reportErr
	}
	_, reportErr := fmt.Fprintf(opts.Stderr,
		"self-update skew: installed supervisor is %s, supervised daemon is %s; keeping existing binary\n",
		formatVersionInfo(opts.supervisor), formatVersionInfo(active))
	return reportErr
}

func sameVersionInfo(left, right versionInfo) bool {
	return left.Version == right.Version &&
		(commitsEqual(left.Commit, right.Commit) || strings.TrimSpace(left.Commit) == strings.TrimSpace(right.Commit))
}

func formatVersionInfo(info versionInfo) string {
	return fmt.Sprintf("%s (commit %s)", info.Version, info.Commit)
}

func pendingRequest(root string) (Request, bool, error) {
	request, err := readRequest(root)
	if errors.Is(err, os.ErrNotExist) {
		return Request{}, false, nil
	}
	if err == nil {
		err = validateRequest(root, request)
	}
	return request, err == nil, err
}

func validateRequest(root string, request Request) error {
	if request.Owner == "" || request.Repository == "" || request.Target == "" || request.HealthTicks < 1 {
		return errors.New("self-update request is missing required fields")
	}
	switch request.Status {
	case "requested", "draining", "activating", "monitoring", "healthy", "rollback":
	default:
		return fmt.Errorf("unsupported self-update request status %q", request.Status)
	}
	timeout, timeoutErr := time.ParseDuration(request.HealthTimeout)
	if timeoutErr != nil || timeout <= 0 {
		return errors.New("self-update request has an invalid health window")
	}
	staged, _, err := stagedLocation(root, request.StagedPath)
	if err != nil {
		return err
	}
	if request.Status == "requested" || request.Status == "draining" || request.Status == "activating" {
		info, err := os.Lstat(staged)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("staged binary %q is not a regular file", staged)
		}
	}
	return nil
}

func stagedLocation(root, stagedPath string) (string, string, error) {
	stageRoot, err := filepath.Abs(stagingDir(root))
	if err != nil {
		return "", "", err
	}
	staged, err := filepath.Abs(stagedPath)
	if err != nil {
		return "", "", err
	}
	relative, err := filepath.Rel(stageRoot, staged)
	if err != nil || relative == "." || !filepath.IsLocal(relative) {
		return "", "", fmt.Errorf("staged binary %q is outside %s", stagedPath, stageRoot)
	}
	return staged, relative, nil
}

func removeCompletedStaging(root, stagedPath string) error {
	_, relative, err := stagedLocation(root, stagedPath)
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(stagingDir(root), strings.SplitN(relative, string(filepath.Separator), 2)[0]))
}
func rejectInvalidRequest(opts SupervisorOptions, requestErr error) error {
	_, reportErr := fmt.Fprintf(opts.Stderr, "ignoring invalid self-update request: %v\n", requestErr)
	if reportErr != nil {
		reportErr = fmt.Errorf("report invalid self-update request: %w", reportErr)
	}
	rejected := fmt.Sprintf("%s.invalid.%d", requestPath(opts.Root), time.Now().UTC().UnixNano())
	if err := os.Rename(requestPath(opts.Root), rejected); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(reportErr, fmt.Errorf("quarantine invalid self-update request: %w", err))
	}
	return reportErr
}

// RequestDaemonStop drops a pending drain-shutdown request for the live
// `goobers up` daemon rooted at root. The daemon's supervisor-stop sweep
// (cmd/goobers/up.go, ConsumeStopRequest below) picks it up on its next
// delegationSweepInterval tick and drives the identical drain path
// SIGINT/SIGTERM already trigger. Originally internal to the self-update
// supervisor's own stop-for-restart flow; exported so `goobers down`
// (#2072) — a plain, non-restarting shutdown request — can reuse the exact
// same file-based mechanism rather than inventing a second one: the
// daemon's response is identical either way, since nothing about restarting
// is encoded in this request file itself (that orchestration lives entirely
// in the supervisor process, not here).
func RequestDaemonStop(root string) error {
	if err := os.MkdirAll(updatesDir(root), 0o755); err != nil {
		return err
	}
	if err := journal.WriteFileAtomic(stopRequestPath(root), nil, 0o600); err != nil {
		return fmt.Errorf("request daemon drain: %w", err)
	}
	return nil
}

// ConsumeStopRequest removes a pending daemon drain request.
func ConsumeStopRequest(root string) (bool, error) {
	err := os.Remove(stopRequestPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
func stopForService(process process, opts SupervisorOptions) error {
	if err := RequestDaemonStop(opts.Root); err != nil {
		return err
	}
	err := waitOrKill(process, opts.DrainTimeout)
	_ = os.Remove(stopRequestPath(opts.Root))
	return err
}
func waitOrKill(process process, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-process.Done():
		return err
	case <-timer.C:
		if err := process.Kill(); err != nil {
			return err
		}
		<-process.Done()
		return nil
	}
}
func terminateProcess(process process, timeout time.Duration) error {
	if process == nil {
		return nil
	}
	select {
	case <-process.Done():
		return nil
	default:
	}
	if err := process.Kill(); err != nil {
		return err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-process.Done():
		return nil
	case <-timer.C:
		return errors.New("timed out waiting for supervised daemon to terminate")
	}
}
func activateCandidate(opts SupervisorOptions, request *Request) error {
	current, previous := currentBinary(opts.Root, opts.GOOS), previousBinary(opts.Root, opts.GOOS)
	if err := copyExecutable(current, previous); err != nil {
		return fmt.Errorf("retain previous binary: %w", err)
	}
	request.RollbackReady = true
	if err := writeRequest(opts.Root, *request); err != nil {
		return fmt.Errorf("record retained previous binary: %w", err)
	}
	return copyExecutable(request.StagedPath, current)
}
func restorePrevious(opts SupervisorOptions) error {
	if err := copyExecutable(previousBinary(opts.Root, opts.GOOS), currentBinary(opts.Root, opts.GOOS)); err != nil {
		return fmt.Errorf("restore retained previous binary: %w", err)
	}
	return nil
}
func copyExecutable(source, destination string) (retErr error) {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", source)
	}
	return writeExecutable(destination, file)
}
func readHeartbeat(root string) (time.Time, error) {
	return daemonstate.Read(filepath.Join(root, "scheduler", "up.lock"))
}
func ensureUpdateEvent(root string, log *journal.InstanceLog, eventType journal.EventType, request Request, reason string) error {
	events, err := journal.ReadInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		return err
	}
	requestedAt := request.RequestedAt.UTC().Format(time.RFC3339Nano)
	for _, event := range events {
		if event.Type == eventType && event.RunID == request.RunID &&
			event.Runner["target"] == request.Target &&
			event.Runner["requestedAt"] == requestedAt {
			return nil
		}
	}
	return appendUpdateEvent(log, eventType, request, reason)
}
func appendUpdateEvent(log *journal.InstanceLog, eventType journal.EventType, request Request, reason string) error {
	return log.Append(journal.Event{
		Type: eventType, Reason: reason, RunID: request.RunID,
		Runner: map[string]any{
			"policy": request.Policy, "target": request.Target,
			"repository":  request.Owner + "/" + request.Repository,
			"requestedAt": request.RequestedAt.UTC().Format(time.RFC3339Nano),
		},
	})
}
