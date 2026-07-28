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

	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/proc"
)

const defaultDrainTimeout = 40 * time.Second

type Escalator interface {
	Escalate(context.Context, Request, string) error
}

type Process interface {
	Done() <-chan error
	Kill() error
}

type Launcher interface {
	Start(string, string, io.Writer, io.Writer) (Process, error)
}

type SupervisorOptions struct {
	Root           string
	GOOS           string
	Launcher       Launcher
	Escalator      Escalator
	Stdout, Stderr io.Writer
	PollInterval   time.Duration
	DrainTimeout   time.Duration
}

type execLauncher struct{}

type execProcess struct {
	tree *proc.Tree
	done chan error
}

func (execLauncher) Start(binary, root string, stdout, stderr io.Writer) (Process, error) {
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
	process, err := opts.Launcher.Start(CurrentBinary(opts.Root, opts.GOOS), opts.Root, opts.Stdout, opts.Stderr)
	if err != nil {
		return fmt.Errorf("start supervised daemon: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, terminateProcess(process, opts.DrainTimeout)) }()
	if pending && request.Status == "rollback" {
		if err := escalateRollback(ctx, opts, log, request); err != nil {
			return err
		}
		pending = false
	}
	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()
	for {
		if pending {
			process, err = performUpdate(ctx, opts, log, process, &request)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return stopForService(process, opts)
				}
				return err
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
	process Process,
	request *Request,
) (Process, error) {
	if request.Status == "requested" {
		if err := ensureUpdateEvent(opts.Root, log, journal.EventDaemonUpdateDrainStarted, *request, "draining before binary handoff"); err != nil {
			return process, err
		}
		if err := setStatus(opts.Root, request, "draining"); err != nil {
			return process, err
		}
	}
	if err := requestDaemonStop(opts.Root); err != nil {
		return process, err
	}
	if err := waitOrKill(process, opts.DrainTimeout); err != nil {
		return process, fmt.Errorf("daemon failed while draining for self-update: %w", err)
	}
	_ = os.Remove(StopRequestPath(opts.Root))

	candidate, baseline, err := startCandidate(opts, log, request)
	if err != nil {
		if request.Status != "monitoring" {
			return candidate, err
		}
		return rollbackAndRestart(ctx, opts, log, request, candidate, err.Error())
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
	return rollbackAndRestart(ctx, opts, log, request, candidate, reason)
}

func monitorCandidate(
	ctx context.Context,
	opts SupervisorOptions,
	log *journal.InstanceLog,
	process Process,
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

func startCandidate(opts SupervisorOptions, log *journal.InstanceLog, request *Request) (Process, time.Time, error) {
	if err := setStatus(opts.Root, request, "activating"); err != nil {
		return nil, time.Time{}, err
	}
	if err := activateCandidate(opts, *request); err != nil {
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
	process, err := opts.Launcher.Start(CurrentBinary(opts.Root, opts.GOOS), opts.Root, opts.Stdout, opts.Stderr)
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
		if _, err := os.Stat(PreviousBinary(opts.Root, opts.GOOS)); err == nil {
			if err = restorePrevious(opts); err != nil {
				return true, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return true, err
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
	ctx context.Context,
	opts SupervisorOptions,
	log *journal.InstanceLog,
	request *Request,
	process Process,
	reason string,
) (Process, error) {
	if process != nil {
		if err := requestDaemonStop(opts.Root); err != nil {
			return nil, err
		}
		if err := waitOrKill(process, opts.DrainTimeout); err != nil {
			return nil, err
		}
		_ = os.Remove(StopRequestPath(opts.Root))
	}
	if err := restorePrevious(opts); err != nil {
		return nil, err
	}
	if err := markRollback(opts.Root, log, request, reason); err != nil {
		return nil, err
	}
	restored, err := opts.Launcher.Start(CurrentBinary(opts.Root, opts.GOOS), opts.Root, opts.Stdout, opts.Stderr)
	if err != nil {
		return nil, fmt.Errorf("restart retained previous binary: %w", err)
	}
	return restored, escalateRollback(ctx, opts, log, *request)
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
	return WriteRequest(root, *request)
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
	if err := os.Remove(RequestPath(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("complete self-update request: %w", err)
	}
	return nil
}

func defaultSupervisorOptions(opts SupervisorOptions) SupervisorOptions {
	opts.GOOS = valueOr(opts.GOOS, runtime.GOOS)
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
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
	return opts
}

func ensureCurrentBinary(opts SupervisorOptions) error {
	current := CurrentBinary(opts.Root, opts.GOOS)
	if _, err := os.Stat(current); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve supervisor executable: %w", err)
	}
	return copyExecutable(executable, current)
}

func pendingRequest(root string) (Request, bool, error) {
	request, err := ReadRequest(root)
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
	stageRoot, err := filepath.Abs(StagingDir(root))
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
	return os.RemoveAll(filepath.Join(StagingDir(root), strings.SplitN(relative, string(filepath.Separator), 2)[0]))
}

func rejectInvalidRequest(opts SupervisorOptions, requestErr error) error {
	fmt.Fprintf(opts.Stderr, "ignoring invalid self-update request: %v\n", requestErr)
	rejected := fmt.Sprintf("%s.invalid.%d", RequestPath(opts.Root), time.Now().UTC().UnixNano())
	if err := os.Rename(RequestPath(opts.Root), rejected); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("quarantine invalid self-update request: %w", err)
	}
	return nil
}

func requestDaemonStop(root string) error {
	if err := os.MkdirAll(UpdatesDir(root), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(StopRequestPath(root), nil, 0o600); err != nil {
		return fmt.Errorf("request daemon drain: %w", err)
	}
	return nil
}

func ConsumeStopRequest(root string) (bool, error) {
	err := os.Remove(StopRequestPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func stopForService(process Process, opts SupervisorOptions) error {
	if err := requestDaemonStop(opts.Root); err != nil {
		return err
	}
	err := waitOrKill(process, opts.DrainTimeout)
	_ = os.Remove(StopRequestPath(opts.Root))
	return err
}

func waitOrKill(process Process, timeout time.Duration) error {
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

func terminateProcess(process Process, timeout time.Duration) error {
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

func activateCandidate(opts SupervisorOptions, request Request) error {
	current, previous := CurrentBinary(opts.Root, opts.GOOS), PreviousBinary(opts.Root, opts.GOOS)
	if err := copyExecutable(current, previous); err != nil {
		return fmt.Errorf("retain previous binary: %w", err)
	}
	return copyExecutable(request.StagedPath, current)
}

func restorePrevious(opts SupervisorOptions) error {
	if err := copyExecutable(PreviousBinary(opts.Root, opts.GOOS), CurrentBinary(opts.Root, opts.GOOS)); err != nil {
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
