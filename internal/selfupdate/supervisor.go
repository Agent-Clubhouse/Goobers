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
	"github.com/goobers/goobers/internal/platform/durability"
)

const (
	defaultDrainTimeout     = 40 * time.Second
	escalationRetryInterval = 30 * time.Second
)

// Escalator files an operator-visible issue after an automatic rollback.
type Escalator interface {
	Escalate(context.Context, Request, string) error
}

func rejectInvalidRequest(opts SupervisorOptions, log *journal.InstanceLog, requestErr error) error {
	if err := log.Append(journal.Event{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: "self_update_request_invalid", Message: requestErr.Error()},
	}); err != nil {
		return err
	}
	path := RequestPath(opts.Root)
	rejected := fmt.Sprintf("%s.invalid.%d", path, opts.Now().UTC().UnixNano())
	if err := os.Rename(path, rejected); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("quarantine invalid self-update request: %w", err)
	}
	return nil
}

func filesEqual(left, right string) (bool, error) {
	leftData, err := os.ReadFile(left)
	if err != nil {
		return false, err
	}
	rightData, err := os.ReadFile(right)
	if err != nil {
		return false, err
	}
	return string(leftData) == string(rightData), nil
}

// Process is one supervised daemon child.
type Process interface {
	Done() <-chan error
	Kill() error
}

// Launcher starts a daemon child from a selected binary.
type Launcher interface {
	Start(string, string, io.Writer, io.Writer) (Process, error)
}

// SupervisorOptions configure the stable service host.
type SupervisorOptions struct {
	Root           string
	HostExecutable string
	GOOS           string
	Launcher       Launcher
	Escalator      Escalator
	Stdout         io.Writer
	Stderr         io.Writer
	PollInterval   time.Duration
	DrainTimeout   time.Duration
	Now            func() time.Time
}

type execLauncher struct{}

type execProcess struct {
	command *exec.Cmd
	done    chan error
}

func (execLauncher) Start(binary, root string, stdout, stderr io.Writer) (Process, error) {
	command := exec.Command(binary, "up", root)
	command.Stdout = stdout
	command.Stderr = stderr
	command.Env = os.Environ()
	if err := command.Start(); err != nil {
		return nil, err
	}
	process := &execProcess{command: command, done: make(chan error, 1)}
	go func() {
		process.done <- command.Wait()
		close(process.done)
	}()
	return process, nil
}

func (p *execProcess) Done() <-chan error { return p.done }
func (p *execProcess) Kill() error        { return p.command.Process.Kill() }

// RunSupervisor launches the mutable binary and owns drain, restart, health,
// rollback, and escalation around durable update requests.
func RunSupervisor(ctx context.Context, opts SupervisorOptions) (retErr error) {
	opts = defaultSupervisorOptions(opts)
	if opts.Root == "" || opts.HostExecutable == "" {
		return errors.New("supervisor instance root and host executable are required")
	}
	if err := ensureCurrentBinary(opts); err != nil {
		return err
	}
	log, _, err := journal.OpenInstanceLog(filepath.Join(opts.Root, "scheduler"))
	if err != nil {
		return fmt.Errorf("open supervisor journal: %w", err)
	}
	defer func() {
		if err := log.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close supervisor journal: %w", err))
		}
	}()

	request, pending, err := pendingRequest(opts.Root)
	if err != nil {
		if err := rejectInvalidRequest(opts, log, err); err != nil {
			return err
		}
		pending = false
	}
	if pending {
		switch request.Status {
		case "healthy":
			if err := finalizeHealthyUpdate(opts, log, request); err != nil {
				return err
			}
			pending = false
		case "activating":
			if err := recoverInterruptedActivation(opts, log, &request); err != nil {
				return err
			}
		case "rollback":
			if err := restorePrevious(opts); err != nil {
				return err
			}
			if err := ensureUpdateEvent(opts, log, journal.EventDaemonUpdateRolledBack, request, request.Reason); err != nil {
				return err
			}
		case "monitoring":
			if err := restorePrevious(opts); err != nil {
				return err
			}
			request.Status = "rollback"
			request.Reason = "stable supervisor restarted before the candidate completed its health window"
			if err := WriteRequest(opts.Root, request); err != nil {
				return err
			}
			if err := ensureUpdateEvent(opts, log, journal.EventDaemonUpdateRolledBack, request, request.Reason); err != nil {
				return err
			}
		}
	}

	process, err := opts.Launcher.Start(CurrentBinary(opts.Root, opts.GOOS), opts.Root, opts.Stdout, opts.Stderr)
	if err != nil {
		return fmt.Errorf("start supervised daemon: %w", err)
	}
	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()

	var (
		monitorStarted time.Time
		lastHeartbeat  time.Time
		baselineSet    bool
		cleanTicks     int
		lastEscalation time.Time
		lastCompletion time.Time
		drainStarted   time.Time
		drainForced    bool
	)
	if pending && request.Status == "draining" {
		drainStarted = opts.Now()
		if err := requestDaemonStop(opts.Root); err != nil {
			return err
		}
	}

	for {
		select {
		case <-ctx.Done():
			return stopForService(process, opts)
		case childErr := <-process.Done():
			if pending && request.Status == "draining" {
				_ = os.Remove(StopRequestPath(opts.Root))
				if childErr != nil && !drainForced {
					return fmt.Errorf("daemon failed while draining for self-update: %w", childErr)
				}
				request.Status = "activating"
				if err := WriteRequest(opts.Root, request); err != nil {
					return err
				}
				if err := activateCandidate(opts, request); err != nil {
					return err
				}
				request.Status = "monitoring"
				if err := WriteRequest(opts.Root, request); err != nil {
					return err
				}
				lastHeartbeat, err = daemonstate.Read(filepath.Join(opts.Root, "scheduler", "up.lock"))
				if errors.Is(err, os.ErrNotExist) {
					lastHeartbeat = time.Time{}
				} else if err != nil {
					process, err = rollbackAndRestart(ctx, opts, log, &request, nil, "read pre-start candidate heartbeat baseline: "+err.Error())
					if err != nil {
						if process == nil {
							return err
						}
						_ = appendSupervisorError(log, request, err)
						request.Status = "rollback"
						pending = true
						continue
					}
					pending = false
					continue
				}
				process, err = opts.Launcher.Start(CurrentBinary(opts.Root, opts.GOOS), opts.Root, opts.Stdout, opts.Stderr)
				if err != nil {
					process, err = rollbackAndRestart(ctx, opts, log, &request, nil, "candidate binary failed to start: "+err.Error())
					if err != nil {
						if process == nil {
							return err
						}
						_ = appendSupervisorError(log, request, err)
						request.Status = "rollback"
						pending = true
						continue
					}
					pending = false
					continue
				}
				if err := appendUpdateEvent(log, journal.EventDaemonUpdateRestarted, request, "candidate binary started"); err != nil {
					_ = process.Kill()
					return err
				}
				monitorStarted = opts.Now()
				baselineSet = false
				cleanTicks = 0
				continue
			}
			if pending && request.Status == "monitoring" {
				reason := "candidate daemon exited before completing its health window"
				if childErr != nil {
					reason += ": " + childErr.Error()
				}
				process, err = rollbackAndRestart(ctx, opts, log, &request, nil, reason)
				if err != nil {
					if process == nil {
						return err
					}
					_ = appendSupervisorError(log, request, err)
					request.Status = "rollback"
					pending = true
					continue
				}
				pending = false
				continue
			}
			if childErr == nil {
				return errors.New("supervised daemon exited unexpectedly")
			}
			return fmt.Errorf("supervised daemon exited: %w", childErr)
		case <-ticker.C:
			if !pending {
				var readErr error
				request, pending, readErr = pendingRequest(opts.Root)
				if readErr != nil {
					if err := rejectInvalidRequest(opts, log, readErr); err != nil {
						return err
					}
					pending = false
					continue
				}
				if !pending {
					continue
				}
			}
			switch request.Status {
			case "requested":
				if err := appendUpdateEvent(log, journal.EventDaemonUpdateDrainStarted, request, "draining before binary handoff"); err != nil {
					return err
				}
				request.Status = "draining"
				drainStarted = opts.Now()
				drainForced = false
				if err := WriteRequest(opts.Root, request); err != nil {
					return err
				}
				if err := requestDaemonStop(opts.Root); err != nil {
					return err
				}
			case "monitoring":
				timeout, err := time.ParseDuration(request.HealthTimeout)
				if err != nil || timeout <= 0 {
					process, err = rollbackAndRestart(ctx, opts, log, &request, process, "candidate request has invalid health timeout")
					if err != nil {
						if process == nil {
							return err
						}
						_ = appendSupervisorError(log, request, err)
						request.Status = "rollback"
						pending = true
						continue
					}
					pending = false
					continue
				}
				if monitorStarted.IsZero() {
					monitorStarted = opts.Now()
				}
				if opts.Now().Sub(monitorStarted) > timeout {
					process, err = rollbackAndRestart(ctx, opts, log, &request, process, "candidate did not complete its bounded health window")
					if err != nil {
						if process == nil {
							return err
						}
						_ = appendSupervisorError(log, request, err)
						request.Status = "rollback"
						pending = true
						continue
					}
					pending = false
					continue
				}
				heartbeat, err := daemonstate.Read(filepath.Join(opts.Root, "scheduler", "up.lock"))
				if err != nil {
					if !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "no such file") {
						process, err = rollbackAndRestart(ctx, opts, log, &request, process, "read candidate heartbeat: "+err.Error())
						if err != nil {
							if process == nil {
								return err
							}
							_ = appendSupervisorError(log, request, err)
							request.Status = "rollback"
							pending = true
							continue
						}
						pending = false
					}
					continue
				}
				if !baselineSet {
					if heartbeat.After(lastHeartbeat) {
						lastHeartbeat = heartbeat
						baselineSet = true
					}
					continue
				}
				if heartbeat.After(lastHeartbeat) {
					lastHeartbeat = heartbeat
					cleanTicks++
				}
				if cleanTicks >= request.HealthTicks {
					request.Status = "healthy"
					if err := WriteRequest(opts.Root, request); err != nil {
						return err
					}
					lastCompletion = time.Time{}
				}
			case "healthy":
				if !lastCompletion.IsZero() && opts.Now().Sub(lastCompletion) < escalationRetryInterval {
					continue
				}
				lastCompletion = opts.Now()
				if err := finalizeHealthyUpdate(opts, log, request); err != nil {
					_ = appendCompletionError(log, request, err)
					continue
				}
				pending = false
			case "rollback":
				if !lastEscalation.IsZero() && opts.Now().Sub(lastEscalation) < escalationRetryInterval {
					continue
				}
				lastEscalation = opts.Now()
				if err := escalateRollback(ctx, opts, log, request); err != nil {
					_ = appendSupervisorError(log, request, err)
					continue
				}
				pending = false
			case "draining":
				if drainStarted.IsZero() {
					drainStarted = opts.Now()
				}
				if !drainForced && opts.Now().Sub(drainStarted) >= opts.DrainTimeout {
					if err := process.Kill(); err != nil {
						return fmt.Errorf("kill daemon after self-update drain timeout: %w", err)
					}
					drainForced = true
				}
			default:
				return fmt.Errorf("unsupported self-update request status %q", request.Status)
			}
		}
	}
}

func defaultSupervisorOptions(opts SupervisorOptions) SupervisorOptions {
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
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
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = defaultDrainTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return opts
}

func ensureCurrentBinary(opts SupervisorOptions) error {
	current := CurrentBinary(opts.Root, opts.GOOS)
	if _, err := os.Stat(current); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect supervised binary: %w", err)
	}
	if err := copyExecutable(opts.HostExecutable, current); err != nil {
		return fmt.Errorf("initialize supervised binary: %w", err)
	}
	return nil
}

func pendingRequest(root string) (Request, bool, error) {
	request, err := ReadRequest(root)
	if errors.Is(err, os.ErrNotExist) {
		return Request{}, false, nil
	}
	if err != nil {
		return Request{}, false, err
	}
	if err := validateRequest(root, request); err != nil {
		return Request{}, false, err
	}
	return request, true, nil
}

func validateRequest(root string, request Request) error {
	if request.Owner == "" || request.Repository == "" || request.Target == "" {
		return errors.New("self-update request is missing repository or target")
	}
	if request.HealthTicks < 1 {
		return errors.New("self-update request health ticks must be positive")
	}
	switch request.Status {
	case "requested", "draining", "activating", "monitoring", "healthy", "rollback":
	default:
		return fmt.Errorf("unsupported self-update request status %q", request.Status)
	}
	timeout, err := time.ParseDuration(request.HealthTimeout)
	if err != nil {
		return fmt.Errorf("self-update request health timeout: %w", err)
	}
	if timeout <= 0 {
		return errors.New("self-update request health timeout must be positive")
	}
	heartbeatInterval, err := time.ParseDuration(request.HeartbeatInterval)
	if err != nil || heartbeatInterval <= 0 {
		return errors.New("self-update request heartbeat interval must be positive")
	}
	if timeout < time.Duration(request.HealthTicks+1)*heartbeatInterval {
		return errors.New("self-update request health timeout is shorter than its heartbeat window")
	}
	staged, _, err := stagedLocation(root, request.StagedPath)
	if err != nil {
		return err
	}
	if request.Status == "requested" || request.Status == "draining" || request.Status == "activating" {
		info, err := os.Stat(staged)
		if err != nil {
			return fmt.Errorf("inspect staged binary: %w", err)
		}
		if !info.Mode().IsRegular() {
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
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
		return "", "", fmt.Errorf("staged binary %q is outside %s", stagedPath, stageRoot)
	}
	return staged, relative, nil
}

func removeCompletedStaging(root, stagedPath string) error {
	_, relative, err := stagedLocation(root, stagedPath)
	if err != nil {
		return err
	}
	stageName := strings.SplitN(relative, string(filepath.Separator), 2)[0]
	return os.RemoveAll(filepath.Join(StagingDir(root), stageName))
}

func requestDaemonStop(root string) error {
	if err := os.MkdirAll(UpdatesDir(root), 0o755); err != nil {
		return fmt.Errorf("create update directory: %w", err)
	}
	file, err := os.OpenFile(StopRequestPath(root), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("request daemon drain: %w", err)
	}
	return file.Close()
}

// ConsumeStopRequest atomically acknowledges a supervisor drain request.
func ConsumeStopRequest(root string) (bool, error) {
	err := os.Remove(StopRequestPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("consume supervisor stop request: %w", err)
	}
	return true, nil
}

func stopForService(process Process, opts SupervisorOptions) error {
	if err := requestDaemonStop(opts.Root); err != nil {
		return err
	}
	timer := time.NewTimer(opts.DrainTimeout)
	defer timer.Stop()
	select {
	case err := <-process.Done():
		_ = os.Remove(StopRequestPath(opts.Root))
		if err != nil {
			return fmt.Errorf("daemon failed during service stop: %w", err)
		}
		return nil
	case <-timer.C:
		if err := process.Kill(); err != nil {
			return fmt.Errorf("kill daemon after drain timeout: %w", err)
		}
		<-process.Done()
		_ = os.Remove(StopRequestPath(opts.Root))
		return errors.New("daemon drain timed out")
	}
}

func activateCandidate(opts SupervisorOptions, request Request) error {
	return activateCandidateWithCopier(opts, request, copyExecutable)
}

func activateCandidateWithCopier(opts SupervisorOptions, request Request, copyFile func(string, string) error) error {
	current := CurrentBinary(opts.Root, opts.GOOS)
	previous := PreviousBinary(opts.Root, opts.GOOS)
	same, err := filesEqual(current, request.StagedPath)
	if err != nil {
		return fmt.Errorf("compare active and staged binaries: %w", err)
	}
	if same {
		if _, err := os.Stat(previous); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect retained previous binary: %w", err)
		}
	}
	if err := copyFile(current, previous); err != nil {
		return fmt.Errorf("retain previous binary: %w", err)
	}
	if err := copyFile(request.StagedPath, current); err != nil {
		return fmt.Errorf("activate staged binary: %w", err)
	}
	return nil
}

func restorePrevious(opts SupervisorOptions) error {
	previous := PreviousBinary(opts.Root, opts.GOOS)
	if _, err := os.Stat(previous); err != nil {
		return fmt.Errorf("inspect retained previous binary: %w", err)
	}
	if err := copyExecutable(previous, CurrentBinary(opts.Root, opts.GOOS)); err != nil {
		return fmt.Errorf("restore retained previous binary: %w", err)
	}
	return nil
}

func recoverInterruptedActivation(opts SupervisorOptions, log *journal.InstanceLog, request *Request) error {
	currentIsCandidate, err := filesEqual(CurrentBinary(opts.Root, opts.GOOS), request.StagedPath)
	if err != nil {
		return fmt.Errorf("inspect interrupted candidate activation: %w", err)
	}
	if currentIsCandidate {
		if _, err := os.Stat(PreviousBinary(opts.Root, opts.GOOS)); err == nil {
			if err := restorePrevious(opts); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect retained previous binary: %w", err)
		}
	}
	request.Status = "rollback"
	request.Reason = "stable supervisor restarted during candidate activation"
	if err := WriteRequest(opts.Root, *request); err != nil {
		return err
	}
	return ensureUpdateEvent(opts, log, journal.EventDaemonUpdateRolledBack, *request, request.Reason)
}

func rollbackAndRestart(ctx context.Context, opts SupervisorOptions, log *journal.InstanceLog, request *Request, process Process, reason string) (Process, error) {
	if process != nil {
		if err := requestDaemonStop(opts.Root); err != nil {
			return nil, err
		}
		timer := time.NewTimer(opts.DrainTimeout)
		select {
		case <-process.Done():
		case <-timer.C:
			if err := process.Kill(); err != nil {
				timer.Stop()
				return nil, fmt.Errorf("kill unhealthy candidate: %w", err)
			}
			<-process.Done()
		}
		timer.Stop()
		_ = os.Remove(StopRequestPath(opts.Root))
	}
	if err := restorePrevious(opts); err != nil {
		return nil, err
	}
	request.Status = "rollback"
	request.Reason = reason
	if err := WriteRequest(opts.Root, *request); err != nil {
		return nil, err
	}
	if err := ensureUpdateEvent(opts, log, journal.EventDaemonUpdateRolledBack, *request, reason); err != nil {
		return nil, err
	}
	restored, err := opts.Launcher.Start(CurrentBinary(opts.Root, opts.GOOS), opts.Root, opts.Stdout, opts.Stderr)
	if err != nil {
		return nil, fmt.Errorf("restart retained previous binary: %w", err)
	}
	if err := escalateRollback(ctx, opts, log, *request); err != nil {
		return restored, err
	}
	return restored, nil
}

func escalateRollback(ctx context.Context, opts SupervisorOptions, log *journal.InstanceLog, request Request) error {
	if opts.Escalator == nil {
		return errors.New("self-update rollback requires an escalation provider")
	}
	if err := opts.Escalator.Escalate(ctx, request, request.Reason); err != nil {
		return fmt.Errorf("create self-update rollback escalation: %w", err)
	}
	if err := appendUpdateEvent(log, journal.EventDaemonUpdateEscalated, request, "rollback escalation issue created"); err != nil {
		return err
	}
	if err := removeCompletedStaging(opts.Root, request.StagedPath); err != nil {
		return fmt.Errorf("clean up rolled-back self-update staging: %w", err)
	}
	if err := os.Remove(RequestPath(opts.Root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("complete rolled-back self-update request: %w", err)
	}
	return nil
}

func finalizeHealthyUpdate(opts SupervisorOptions, log *journal.InstanceLog, request Request) error {
	if err := ensureUpdateEvent(opts, log, journal.EventDaemonUpdateHealthy, request, "candidate completed clean heartbeat window"); err != nil {
		return err
	}
	if err := removeCompletedStaging(opts.Root, request.StagedPath); err != nil {
		return fmt.Errorf("clean up completed self-update staging: %w", err)
	}
	if err := os.Remove(RequestPath(opts.Root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("complete self-update request: %w", err)
	}
	return nil
}

func ensureUpdateEvent(opts SupervisorOptions, log *journal.InstanceLog, eventType journal.EventType, request Request, reason string) error {
	recorded, err := updateEventRecorded(opts.Root, eventType, request)
	if err != nil {
		return fmt.Errorf("inspect completed self-update journal: %w", err)
	}
	if !recorded {
		if err := appendUpdateEvent(log, eventType, request, reason); err != nil {
			return err
		}
	}
	return nil
}

func updateEventRecorded(root string, eventType journal.EventType, request Request) (bool, error) {
	events, err := journal.ReadInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		return false, err
	}
	requestedAt := request.RequestedAt.UTC().Format(time.RFC3339Nano)
	for _, event := range events {
		if event.Type != eventType || event.RunID != request.RunID {
			continue
		}
		target, _ := event.Runner["target"].(string)
		repository, _ := event.Runner["repository"].(string)
		eventRequestedAt, _ := event.Runner["requestedAt"].(string)
		if target == request.Target &&
			repository == request.Owner+"/"+request.Repository &&
			eventRequestedAt == requestedAt {
			return true, nil
		}
	}
	return false, nil
}

func appendUpdateEvent(log *journal.InstanceLog, eventType journal.EventType, request Request, reason string) error {
	return log.Append(journal.Event{
		Type:   eventType,
		Reason: reason,
		RunID:  request.RunID,
		Runner: map[string]any{
			"policy":      request.Policy,
			"target":      request.Target,
			"repository":  request.Owner + "/" + request.Repository,
			"requestedAt": request.RequestedAt.UTC().Format(time.RFC3339Nano),
		},
	})
}

func appendSupervisorError(log *journal.InstanceLog, request Request, err error) error {
	return log.Append(journal.Event{
		Type:  journal.EventError,
		RunID: request.RunID,
		Error: &journal.ErrorDetail{
			Code:    "self_update_escalation_failed",
			Message: err.Error(),
		},
		Runner: map[string]any{"target": request.Target},
	})
}

func appendCompletionError(log *journal.InstanceLog, request Request, err error) error {
	return log.Append(journal.Event{
		Type:  journal.EventError,
		RunID: request.RunID,
		Error: &journal.ErrorDetail{
			Code:    "self_update_completion_failed",
			Message: err.Error(),
		},
		Runner: map[string]any{"target": request.Target},
	})
}

func copyExecutable(source, destination string) (retErr error) {
	sourceFile, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() {
		if err := sourceFile.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close source executable: %w", err))
		}
	}()
	info, err := sourceFile.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", source)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(destination), ".binary-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, fmt.Errorf("remove temporary executable: %w", err))
		}
	}()
	if _, err := io.Copy(temp, sourceFile); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Chmod(0o755); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := durability.ReplaceFile(tempPath, destination); err != nil {
		return err
	}
	return durability.SyncDir(filepath.Dir(destination))
}
