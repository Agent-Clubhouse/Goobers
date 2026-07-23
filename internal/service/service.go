// Package service installs and manages the Goobers daemon with the native
// platform supervisor.
package service

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/packaging"
)

const (
	// Name is the platform service name used by Goobers.
	Name         = "goobers"
	launchdLabel = "com.agent-clubhouse.goobers"

	serviceStartupTimeout  = 10 * time.Second
	serviceStopTimeout     = 50 * time.Second
	serviceStatusInterval  = 100 * time.Millisecond
	serviceReadinessWindow = time.Second
	serviceReadinessChecks = int(serviceReadinessWindow/serviceStatusInterval) + 1
)

// ErrAlreadyInstalled indicates that a Goobers service definition already exists.
var ErrAlreadyInstalled = errors.New("goobers service is already installed")

// Status describes the installed and runtime state of the Goobers service.
type Status struct {
	Platform   string `json:"platform"`
	Supervisor string `json:"supervisor"`
	Installed  bool   `json:"installed"`
	Loaded     bool   `json:"loaded"`
	Running    bool   `json:"running"`
	State      string `json:"state"`
	ConfigPath string `json:"configPath,omitempty"`
}

// CommandRunner executes native supervisor commands.
type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, int, error)
}

// Config supplies the platform details and command runner used by a Manager.
type Config struct {
	GOOS         string
	Executable   string
	InstanceRoot string
	HomeDir      string
	UID          string
	UserName     string
	Runner       CommandRunner
}

// Manager manages the Goobers service through the configured platform supervisor.
type Manager struct {
	config Config
}

// New creates a service manager for instanceRoot on the current platform.
func New(instanceRoot string) (*Manager, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve goobers executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute goobers executable: %w", err)
	}
	instanceRoot, err = filepath.Abs(instanceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute instance root: %w", err)
	}
	home := ""
	if runtime.GOOS != "windows" {
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve user home: %w", err)
		}
	}
	uid := ""
	userName := ""
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		current, currentErr := user.Current()
		if currentErr != nil {
			return nil, fmt.Errorf("resolve current user: %w", currentErr)
		}
		if runtime.GOOS == "darwin" {
			uid = current.Uid
		} else {
			userName = current.Username
		}
	}
	return NewWithConfig(Config{
		GOOS:         runtime.GOOS,
		Executable:   executable,
		InstanceRoot: instanceRoot,
		HomeDir:      home,
		UID:          uid,
		UserName:     userName,
		Runner:       execRunner{},
	})
}

// NewWithConfig creates a service manager with explicitly supplied platform details.
func NewWithConfig(config Config) (*Manager, error) {
	switch config.GOOS {
	case "linux", "darwin", "windows":
	default:
		return nil, fmt.Errorf("service supervision is not supported on %s", config.GOOS)
	}
	if config.Executable == "" {
		return nil, errors.New("goobers executable path is required")
	}
	if config.InstanceRoot == "" {
		return nil, errors.New("instance root is required")
	}
	if config.GOOS != "windows" && config.HomeDir == "" {
		return nil, errors.New("user home is required")
	}
	if config.GOOS == "darwin" && config.UID == "" {
		return nil, errors.New("user id is required on macOS")
	}
	if config.GOOS == "linux" && config.UserName == "" {
		return nil, errors.New("user name is required on Linux")
	}
	if config.Runner == nil {
		return nil, errors.New("command runner is required")
	}
	return &Manager{config: config}, nil
}

// Install registers and starts the Goobers service.
func (m *Manager) Install(ctx context.Context) (Status, error) {
	switch m.config.GOOS {
	case "linux":
		return m.installSystemd(ctx)
	case "darwin":
		return m.installLaunchd(ctx)
	case "windows":
		return m.installWindows(ctx)
	default:
		panic("validated service platform")
	}
}

// Uninstall stops and removes the Goobers service.
func (m *Manager) Uninstall(ctx context.Context) error {
	switch m.config.GOOS {
	case "linux":
		return m.uninstallSystemd(ctx)
	case "darwin":
		return m.uninstallLaunchd(ctx)
	case "windows":
		return m.uninstallWindows(ctx)
	default:
		panic("validated service platform")
	}
}

// Status reports the current Goobers service state.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	switch m.config.GOOS {
	case "linux":
		return m.statusSystemd(ctx)
	case "darwin":
		return m.statusLaunchd(ctx)
	case "windows":
		return m.statusWindows(ctx)
	default:
		panic("validated service platform")
	}
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err == nil {
		return output, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return output, exitErr.ExitCode(), nil
	}
	return output, -1, err
}

func (m *Manager) installSystemd(ctx context.Context) (Status, error) {
	path := m.systemdPath()
	template, err := packaging.ServiceFiles.ReadFile("systemd/goobers.service")
	if err != nil {
		return Status{}, fmt.Errorf("read embedded systemd unit: %w", err)
	}
	content := strings.ReplaceAll(
		string(template),
		"ExecStart=%GOOBERS_BIN% up %INSTANCE_ROOT%",
		"ExecStart="+quoteSystemd(m.config.Executable)+" up "+quoteSystemd(m.config.InstanceRoot),
	)
	content = strings.ReplaceAll(content, "%GOOBERS_BIN%", quoteSystemd(m.config.Executable))
	content = strings.ReplaceAll(content, "%INSTANCE_ROOT%", escapeSystemdSpecifier(m.config.InstanceRoot))
	if err := writeExclusive(path, []byte(content)); err != nil {
		return Status{}, err
	}
	if err := m.runRequired(ctx, "loginctl", "enable-linger", m.config.UserName); err != nil {
		return Status{}, errors.Join(err, removeServiceConfig(path))
	}
	if err := m.runRequired(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return Status{}, errors.Join(err, removeServiceConfig(path))
	}
	if err := m.runRequired(ctx, "systemctl", "--user", "enable", "--now", Name+".service"); err != nil {
		return Status{}, errors.Join(err, m.rollbackSystemd(ctx, path))
	}
	status, err := waitUntilRunning(ctx, m.statusSystemd)
	if err != nil {
		return Status{}, errors.Join(err, m.rollbackSystemd(ctx, path))
	}
	return status, nil
}

func (m *Manager) uninstallSystemd(ctx context.Context) error {
	path := m.systemdPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect systemd unit: %w", err)
	}
	if err := m.stopSystemd(ctx); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove systemd unit: %w", err)
	}
	return m.runRequired(ctx, "systemctl", "--user", "daemon-reload")
}

func (m *Manager) statusSystemd(ctx context.Context) (Status, error) {
	status := Status{
		Platform:   "linux",
		Supervisor: "systemd",
		State:      "not-installed",
		ConfigPath: m.systemdPath(),
	}
	if _, err := os.Stat(status.ConfigPath); errors.Is(err, os.ErrNotExist) {
		return status, nil
	} else if err != nil {
		return Status{}, fmt.Errorf("inspect systemd unit: %w", err)
	}
	status.Installed = true
	output, code, err := m.config.Runner.Run(ctx, "systemctl", "--user", "show",
		"--property=LoadState", "--property=ActiveState", "--property=UnitFileState", Name+".service")
	if err != nil {
		return Status{}, fmt.Errorf("run systemctl: %w", err)
	}
	if code != 0 {
		return Status{}, commandError("systemctl", code, output)
	}
	values := parseProperties(string(output))
	status.Loaded = values["LoadState"] == "loaded"
	status.Running = values["ActiveState"] == "active"
	status.State = values["ActiveState"]
	if status.State == "" {
		status.State = "unknown"
	}
	return status, nil
}

func (m *Manager) installLaunchd(ctx context.Context) (Status, error) {
	path := m.launchdPath()
	template, err := packaging.ServiceFiles.ReadFile("launchd/com.agent-clubhouse.goobers.plist")
	if err != nil {
		return Status{}, fmt.Errorf("read embedded launchd plist: %w", err)
	}
	logDir := filepath.Join(m.config.HomeDir, "Library", "Logs", "Goobers")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return Status{}, fmt.Errorf("create launchd log directory: %w", err)
	}
	content := strings.ReplaceAll(string(template), "%GOOBERS_BIN%", html.EscapeString(m.config.Executable))
	content = strings.ReplaceAll(content, "%INSTANCE_ROOT%", html.EscapeString(m.config.InstanceRoot))
	content = strings.ReplaceAll(content, "%LOG_DIR%", html.EscapeString(logDir))
	if err := writeExclusive(path, []byte(content)); err != nil {
		return Status{}, err
	}
	domain := m.launchdDomain()
	target := domain + "/" + launchdLabel
	if err := m.runRequired(ctx, "launchctl", "bootstrap", domain, path); err != nil {
		return Status{}, errors.Join(err, removeServiceConfig(path))
	}
	if err := m.runRequired(ctx, "launchctl", "enable", target); err != nil {
		return Status{}, errors.Join(err, m.rollbackLaunchd(ctx, path, target))
	}
	if err := m.runRequired(ctx, "launchctl", "kickstart", target); err != nil {
		return Status{}, errors.Join(err, m.rollbackLaunchd(ctx, path, target))
	}
	status, err := waitUntilRunning(ctx, m.statusLaunchd)
	if err != nil {
		return Status{}, errors.Join(err, m.rollbackLaunchd(ctx, path, target))
	}
	return status, nil
}

func (m *Manager) uninstallLaunchd(ctx context.Context) error {
	path := m.launchdPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect launchd plist: %w", err)
	}
	status, err := m.statusLaunchd(ctx)
	if err != nil {
		return err
	}
	if status.Loaded {
		if err := m.stopLaunchd(ctx, m.launchdDomain()+"/"+launchdLabel); err != nil {
			return err
		}
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove launchd plist: %w", err)
	}
	return nil
}

func (m *Manager) statusLaunchd(ctx context.Context) (Status, error) {
	status := Status{
		Platform:   "darwin",
		Supervisor: "launchd",
		State:      "not-installed",
		ConfigPath: m.launchdPath(),
	}
	if _, err := os.Stat(status.ConfigPath); errors.Is(err, os.ErrNotExist) {
		return status, nil
	} else if err != nil {
		return Status{}, fmt.Errorf("inspect launchd plist: %w", err)
	}
	status.Installed = true
	output, code, err := m.config.Runner.Run(ctx, "launchctl", "print", m.launchdDomain()+"/"+launchdLabel)
	if err != nil {
		return Status{}, fmt.Errorf("run launchctl: %w", err)
	}
	if code != 0 {
		if code == 113 || strings.Contains(string(output), "Could not find service") {
			status.State = "stopped"
			return status, nil
		}
		return Status{}, commandError("launchctl", code, output)
	}
	status.Loaded = true
	status.State = launchdState(string(output))
	status.Running = status.State == "running"
	return status, nil
}

func (m *Manager) installWindows(ctx context.Context) (Status, error) {
	status, err := m.statusWindows(ctx)
	if err != nil {
		return Status{}, err
	}
	if status.Installed {
		return Status{}, ErrAlreadyInstalled
	}
	binPath := quoteWindowsCommandArg(m.config.Executable) + " up " + quoteWindowsCommandArg(m.config.InstanceRoot)
	if err := m.runRequired(ctx, "sc.exe", "create", Name,
		"binPath=", binPath,
		"start=", "auto",
		"DisplayName=", "Goobers daemon"); err != nil {
		return Status{}, err
	}
	if err := m.runRequired(ctx, "sc.exe", "description", Name,
		"Goobers agent-workforce daemon (scheduler + local runner)"); err != nil {
		return Status{}, errors.Join(err, m.rollbackWindows(ctx))
	}
	if err := m.runRequired(ctx, "sc.exe", "failure", Name,
		"reset=", "86400",
		"actions=", "restart/5000/restart/30000/restart/60000"); err != nil {
		return Status{}, errors.Join(err, m.rollbackWindows(ctx))
	}
	if err := m.runRequired(ctx, "sc.exe", "failureflag", Name, "1"); err != nil {
		return Status{}, errors.Join(err, m.rollbackWindows(ctx))
	}
	if err := m.runRequired(ctx, "sc.exe", "start", Name); err != nil {
		return Status{}, errors.Join(err, m.rollbackWindows(ctx))
	}
	status, err = waitUntilRunning(ctx, m.statusWindows)
	if err != nil {
		return Status{}, errors.Join(err, m.rollbackWindows(ctx))
	}
	return status, nil
}

func (m *Manager) uninstallWindows(ctx context.Context) error {
	status, err := m.statusWindows(ctx)
	if err != nil {
		return err
	}
	if !status.Installed {
		return nil
	}
	if status.State != "stopped" {
		if err := m.runRequired(ctx, "sc.exe", "stop", Name); err != nil {
			return err
		}
		status, err = waitUntilStopped(ctx, m.statusWindows)
		if err != nil {
			return err
		}
		if !status.Installed {
			return nil
		}
	}
	return m.runRequired(ctx, "sc.exe", "delete", Name)
}

func (m *Manager) statusWindows(ctx context.Context) (Status, error) {
	status := Status{
		Platform:   "windows",
		Supervisor: "windows-service",
		State:      "not-installed",
	}
	output, code, err := m.config.Runner.Run(ctx, "sc.exe", "query", Name)
	if err != nil {
		return Status{}, fmt.Errorf("run sc.exe: %w", err)
	}
	if code != 0 {
		if code == 1060 || strings.Contains(string(output), "1060") {
			return status, nil
		}
		return Status{}, commandError("sc.exe", code, output)
	}
	status.Installed = true
	status.Loaded = true
	status.State = windowsServiceState(string(output))
	status.Running = status.State == "running"
	return status, nil
}

func (m *Manager) runRequired(ctx context.Context, name string, args ...string) error {
	output, code, err := m.config.Runner.Run(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("run %s: %w", name, err)
	}
	if code != 0 {
		return commandError(name, code, output)
	}
	return nil
}

func (m *Manager) rollbackSystemd(ctx context.Context, path string) error {
	if err := m.stopSystemd(ctx); err != nil {
		return err
	}
	removeErr := os.Remove(path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	if removeErr != nil {
		removeErr = fmt.Errorf("remove systemd unit during rollback: %w", removeErr)
	}
	reloadErr := m.runRequired(ctx, "systemctl", "--user", "daemon-reload")
	return errors.Join(removeErr, reloadErr)
}

func (m *Manager) rollbackLaunchd(ctx context.Context, path, target string) error {
	if err := m.stopLaunchd(ctx, target); err != nil {
		return err
	}
	removeErr := os.Remove(path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	if removeErr != nil {
		removeErr = fmt.Errorf("remove launchd plist during rollback: %w", removeErr)
	}
	return removeErr
}

func (m *Manager) rollbackWindows(ctx context.Context) error {
	return m.uninstallWindows(ctx)
}

func (m *Manager) stopSystemd(ctx context.Context) error {
	if err := m.runRequired(ctx, "systemctl", "--user", "disable", "--now", Name+".service"); err != nil {
		return err
	}
	_, err := waitUntilStopped(ctx, m.statusSystemd)
	return err
}

func (m *Manager) stopLaunchd(ctx context.Context, target string) error {
	if err := m.runRequired(ctx, "launchctl", "bootout", target); err != nil {
		return err
	}
	_, err := waitUntilStopped(ctx, m.statusLaunchd)
	return err
}

func (m *Manager) systemdPath() string {
	return filepath.Join(m.config.HomeDir, ".config", "systemd", "user", Name+".service")
}

func (m *Manager) launchdPath() string {
	return filepath.Join(m.config.HomeDir, "Library", "LaunchAgents", launchdLabel+".plist")
}

func (m *Manager) launchdDomain() string {
	return "gui/" + m.config.UID
}

func writeExclusive(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create service configuration directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, os.ErrExist) {
		return ErrAlreadyInstalled
	}
	if err != nil {
		return fmt.Errorf("create service configuration: %w", err)
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(content); err != nil {
		return fmt.Errorf("write service configuration: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync service configuration: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close service configuration: %w", err)
	}
	remove = false
	return nil
}

func removeServiceConfig(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove service configuration during rollback: %w", err)
	}
	return nil
}

func quoteSystemd(value string) string {
	value = escapeSystemdSpecifier(value)
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

func escapeSystemdSpecifier(value string) string {
	return strings.ReplaceAll(value, "%", "%%")
}

func quoteWindowsCommandArg(value string) string {
	var quoted strings.Builder
	quoted.WriteByte('"')
	backslashes := 0
	for _, char := range value {
		switch char {
		case '\\':
			backslashes++
		case '"':
			quoted.WriteString(strings.Repeat(`\`, backslashes*2+1))
			quoted.WriteRune(char)
			backslashes = 0
		default:
			quoted.WriteString(strings.Repeat(`\`, backslashes))
			backslashes = 0
			quoted.WriteRune(char)
		}
	}
	quoted.WriteString(strings.Repeat(`\`, backslashes*2))
	quoted.WriteByte('"')
	return quoted.String()
}

func waitUntilRunning(ctx context.Context, status func(context.Context) (Status, error)) (Status, error) {
	waitCtx, cancel := context.WithTimeout(ctx, serviceStartupTimeout)
	defer cancel()
	ticker := time.NewTicker(serviceStatusInterval)
	defer ticker.Stop()
	var last Status
	consecutiveRunning := 0
	for {
		current, err := status(waitCtx)
		if err != nil {
			return Status{}, err
		}
		last = current
		if !current.Installed {
			return Status{}, errors.New("service registration disappeared while starting")
		}
		if current.Running {
			consecutiveRunning++
			if consecutiveRunning >= serviceReadinessChecks {
				return current, nil
			}
		} else {
			consecutiveRunning = 0
		}
		select {
		case <-waitCtx.Done():
			return Status{}, fmt.Errorf(
				"service did not remain running for %s (last state %s): %w",
				serviceReadinessWindow, last.State, waitCtx.Err(),
			)
		case <-ticker.C:
		}
	}
}

func waitUntilStopped(ctx context.Context, status func(context.Context) (Status, error)) (Status, error) {
	waitCtx, cancel := context.WithTimeout(ctx, serviceStopTimeout)
	defer cancel()
	ticker := time.NewTicker(serviceStatusInterval)
	defer ticker.Stop()
	var last Status
	for {
		current, err := status(waitCtx)
		if err != nil {
			return Status{}, err
		}
		last = current
		if serviceStopped(current) {
			return current, nil
		}
		select {
		case <-waitCtx.Done():
			return Status{}, fmt.Errorf("service did not reach stopped state (last state %s): %w", last.State, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func serviceStopped(status Status) bool {
	if !status.Installed || status.State == "stopped" {
		return true
	}
	return status.Supervisor == "systemd" && (status.State == "inactive" || status.State == "failed")
}

func parseProperties(output string) map[string]string {
	properties := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			properties[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return properties
}

func launchdState(output string) string {
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.TrimSpace(key) == "state" {
			return strings.TrimSpace(value)
		}
	}
	return "loaded"
}

func windowsServiceState(output string) string {
	var numericFields []int
	for _, line := range strings.Split(output, "\n") {
		_, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		numericValue, err := strconv.Atoi(fields[0])
		if err == nil {
			numericFields = append(numericFields, numericValue)
		}
	}
	// sc.exe localizes field labels but always reports service type before numeric state.
	if len(numericFields) >= 2 {
		return windowsServiceStateCode(numericFields[1])
	}
	if len(numericFields) == 1 {
		return windowsServiceStateCode(numericFields[0])
	}
	return "unknown"
}

func windowsServiceStateCode(code int) string {
	switch code {
	case 1:
		return "stopped"
	case 2:
		return "start-pending"
	case 3:
		return "stop-pending"
	case 4:
		return "running"
	case 5:
		return "continue-pending"
	case 6:
		return "pause-pending"
	case 7:
		return "paused"
	default:
		return "unknown"
	}
}

func commandError(name string, code int, output []byte) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s exited with code %d", name, code)
	}
	return fmt.Errorf("%s exited with code %d: %s", name, code, detail)
}
