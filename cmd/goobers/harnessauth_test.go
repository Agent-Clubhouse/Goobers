package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

type authPreflightRunner struct {
	authExit       int
	authTranscript string
	authEnv        []string
	authCommand    []string
	authConfig     bool
}

func (r *authPreflightRunner) Run(_ context.Context, req harness.ProcessRequest) (harness.ProcessResult, error) {
	if slices.Contains(req.Command, "--version") {
		_, err := io.WriteString(req.StdoutCapture, "copilot fixture version\n")
		return harness.ProcessResult{}, err
	}
	r.authCommand = slices.Clone(req.Command)
	r.authEnv = slices.Clone(req.Env)
	if home, ok := envValue(req.Env, "COPILOT_HOME"); ok {
		_, err := os.Stat(filepath.Join(home, "config.json"))
		r.authConfig = err == nil
	}
	return harness.ProcessResult{ExitCode: r.authExit, Transcript: []byte(r.authTranscript)}, nil
}

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

type authStatusAdapter struct {
	*harnesstest.FakeAdapter
	info    harness.AuthInfo
	err     error
	command []string
	env     []string
}

func (a *authStatusAdapter) AuthStatus(context.Context) (harness.AuthInfo, error) {
	return a.info, a.err
}

func (a *authStatusAdapter) AuthCommand(_ context.Context, operation string) ([]string, []string, error) {
	if operation != "login" && operation != "logout" {
		return nil, nil, errors.New("unsupported operation")
	}
	return slices.Clone(a.command), slices.Clone(a.env), nil
}

func TestHarnessAuthCopilotStatusReportsCredentialFreeState(t *testing.T) {
	const secret = "github_pat_secretvalue"
	withHarnessAdapter(t, func(apiv1.Harness, harness.EnvironmentConfig, map[string][]string, func(context.Context) (string, error)) (harness.Adapter, error) {
		return &authStatusAdapter{
			FakeAdapter: &harnesstest.FakeAdapter{AdapterName: "copilot-cli"},
			info: harness.AuthInfo{
				Status:      harness.AuthStatusSignedOut,
				Executable:  "copilot",
				Runner:      "local",
				ProfileDir:  t.TempDir(),
				Remediation: "goobers harness auth copilot login",
			},
		}, nil
	})
	code, stdout, stderr := runArgs(t, "harness", "auth", "copilot", "status")
	if code != 1 {
		t.Fatalf("code = %d, want 1 for signed-out status; stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{"HARNESS copilot auth: signed-out", "executable: copilot", "runner: local", "remediation: goobers harness auth copilot login"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
		t.Fatalf("auth status leaked a token: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestHarnessAuthCopilotStatusAuthenticatedExitsZero(t *testing.T) {
	withHarnessAdapter(t, func(apiv1.Harness, harness.EnvironmentConfig, map[string][]string, func(context.Context) (string, error)) (harness.Adapter, error) {
		return &authStatusAdapter{
			FakeAdapter: &harnesstest.FakeAdapter{AdapterName: "copilot-cli"},
			info: harness.AuthInfo{
				Status:     harness.AuthStatusAuthenticated,
				Executable: "copilot",
				Version:    "copilot version 1.0.0",
				Runner:     "local",
			},
		}, nil
	})
	code, stdout, stderr := runArgs(t, "harness", "auth", "copilot", "status")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "HARNESS copilot auth: authenticated") || strings.Contains(stdout, "remediation:") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestHarnessAuthCopilotLoginDelegatesConfiguredCommand(t *testing.T) {
	previous := runCopilotNativeAuthCommand
	defer func() { runCopilotNativeAuthCommand = previous }()
	var got []string
	runCopilotNativeAuthCommand = func(_ context.Context, command, _ []string, _ io.Writer, _ io.Writer) error {
		got = slices.Clone(command)
		return nil
	}
	withHarnessAdapter(t, func(apiv1.Harness, harness.EnvironmentConfig, map[string][]string, func(context.Context) (string, error)) (harness.Adapter, error) {
		return &authStatusAdapter{
			FakeAdapter: &harnesstest.FakeAdapter{AdapterName: "copilot-cli"},
			command:     []string{"copilot", "login"},
			info:        harness.AuthInfo{Status: harness.AuthStatusAuthenticated, Executable: "copilot", Runner: "local"},
		}, nil
	})
	code, stdout, stderr := runArgs(t, "harness", "auth", "copilot", "login")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !slices.Equal(got, []string{"copilot", "login"}) {
		t.Fatalf("native auth command = %v, want [copilot login]", got)
	}
	if !strings.Contains(stdout, "HARNESS copilot auth: authenticated") {
		t.Fatalf("login did not print verified auth status: %q", stdout)
	}
}

func TestHarnessAuthCopilotLoginFailsWhenVerificationIsNotAuthenticated(t *testing.T) {
	previous := runCopilotNativeAuthCommand
	defer func() { runCopilotNativeAuthCommand = previous }()
	runCopilotNativeAuthCommand = func(_ context.Context, _ []string, _ []string, _ io.Writer, _ io.Writer) error {
		return nil
	}
	withHarnessAdapter(t, func(apiv1.Harness, harness.EnvironmentConfig, map[string][]string, func(context.Context) (string, error)) (harness.Adapter, error) {
		return &authStatusAdapter{
			FakeAdapter: &harnesstest.FakeAdapter{AdapterName: "copilot-cli"},
			command:     []string{"copilot", "login"},
			info:        harness.AuthInfo{Status: harness.AuthStatusSignedOut, Executable: "copilot", Runner: "local"},
		}, nil
	})
	code, stdout, stderr := runArgs(t, "harness", "auth", "copilot", "login")
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "login completed but Copilot authentication is signed-out") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestHarnessAuthCopilotLoginDelegatesLauncherWrappedCommand(t *testing.T) {
	previous := runCopilotNativeAuthCommand
	defer func() { runCopilotNativeAuthCommand = previous }()
	var gotCommand, gotEnv []string
	runCopilotNativeAuthCommand = func(_ context.Context, command, env []string, _ io.Writer, _ io.Writer) error {
		gotCommand = slices.Clone(command)
		gotEnv = slices.Clone(env)
		return nil
	}
	withHarnessAdapter(t, func(apiv1.Harness, harness.EnvironmentConfig, map[string][]string, func(context.Context) (string, error)) (harness.Adapter, error) {
		return &authStatusAdapter{
			FakeAdapter: &harnesstest.FakeAdapter{AdapterName: "copilot-cli"},
			command:     []string{"launcher", "copilot", "auth", "interactive-login"},
			env:         []string{"COPILOT_HOME=C:\\profile"},
			info:        harness.AuthInfo{Status: harness.AuthStatusAuthenticated, Executable: "launcher copilot", Runner: "configured launcher", ProfileDir: "C:\\profile"},
		}, nil
	})
	code, stdout, stderr := runArgs(t, "harness", "auth", "copilot", "login")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !slices.Equal(gotCommand, []string{"launcher", "copilot", "auth", "interactive-login"}) {
		t.Fatalf("command = %v", gotCommand)
	}
	if !slices.Equal(gotEnv, []string{"COPILOT_HOME=C:\\profile"}) {
		t.Fatalf("env = %v", gotEnv)
	}
}

func TestHarnessAuthCopilotLogoutFailsClearlyForDirectNativeHarness(t *testing.T) {
	code, stdout, stderr := runArgs(t, "harness", "auth", "copilot", "logout")
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "logout is not supported") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestHarnessAuthCopilotStatusUsesRealPreflightForPersistedLogin(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"oauthToken":"stored-login"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPILOT_HOME", home)
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := &authPreflightRunner{}
	withHarnessAdapter(t, func(apiv1.Harness, harness.EnvironmentConfig, map[string][]string, func(context.Context) (string, error)) (harness.Adapter, error) {
		return &harness.CopilotAdapter{
			Command:           []string{program},
			Runner:            runner,
			AuthCheckArgs:     copilotAuthCheckArgs,
			ExtraEnvAllowlist: []string{"COPILOT_HOME"},
		}, nil
	})
	code, stdout, stderr := runArgs(t, "harness", "auth", "copilot", "status")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "HARNESS copilot auth: authenticated") || !strings.Contains(stdout, "version: copilot fixture version") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !slices.Contains(runner.authCommand, "-p") || slices.Contains(runner.authCommand, "login") {
		t.Fatalf("auth probe command = %v, want unattended status probe", runner.authCommand)
	}
	authHome, ok := envValue(runner.authEnv, "COPILOT_HOME")
	if !ok || authHome == home {
		t.Fatalf("auth probe COPILOT_HOME = %q, %v; want isolated copy", authHome, ok)
	}
	if !runner.authConfig {
		t.Fatal("persisted login was not copied into isolated preflight home")
	}
}

func TestHarnessAuthCopilotStatusRefusesSignedOutWithoutInteractivePrompt(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := &authPreflightRunner{authExit: 1, authTranscript: "not signed in"}
	withHarnessAdapter(t, func(apiv1.Harness, harness.EnvironmentConfig, map[string][]string, func(context.Context) (string, error)) (harness.Adapter, error) {
		return &harness.CopilotAdapter{Command: []string{program}, Runner: runner, AuthCheckArgs: copilotAuthCheckArgs}, nil
	})
	code, stdout, stderr := runArgs(t, "harness", "auth", "copilot", "status")
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "HARNESS copilot auth: signed-out") || strings.Contains(stderr, "not signed in") {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
	if !slices.Contains(runner.authCommand, "-p") || slices.Contains(runner.authCommand, "login") {
		t.Fatalf("auth probe command = %v, want unattended status probe", runner.authCommand)
	}
}

func TestHarnessAuthCopilotStatusClassifiesExpiredAndInaccessiblePreflightState(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, transcript := range []string{"credential is expired", "stored credential is inaccessible"} {
		t.Run(transcript, func(t *testing.T) {
			runner := &authPreflightRunner{authExit: 1, authTranscript: transcript}
			withHarnessAdapter(t, func(apiv1.Harness, harness.EnvironmentConfig, map[string][]string, func(context.Context) (string, error)) (harness.Adapter, error) {
				return &harness.CopilotAdapter{Command: []string{program}, Runner: runner, AuthCheckArgs: copilotAuthCheckArgs}, nil
			})
			code, stdout, stderr := runArgs(t, "harness", "auth", "copilot", "status")
			if code != 1 || !strings.Contains(stdout, "HARNESS copilot auth: signed-out") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}

func TestHarnessAuthCopilotStatusUsesExplicitCredentialFallback(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := &authPreflightRunner{}
	withHarnessAdapter(t, func(apiv1.Harness, harness.EnvironmentConfig, map[string][]string, func(context.Context) (string, error)) (harness.Adapter, error) {
		return &harness.CopilotAdapter{
			Command:       []string{program},
			Runner:        runner,
			AuthCheckArgs: copilotAuthCheckArgs,
			ModelCredential: func(context.Context) (string, error) {
				return "explicit-token", nil
			},
		}, nil
	})
	code, stdout, stderr := runArgs(t, "harness", "auth", "copilot", "status")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if token, ok := envValue(runner.authEnv, "COPILOT_GITHUB_TOKEN"); !ok || token != "explicit-token" {
		t.Fatalf("auth probe token = %q, %v; want explicit credential fallback", token, ok)
	}
}

func TestHarnessAuthCopilotLoginRefusesNonSelfCopilotPlacement(t *testing.T) {
	root := initDemo(t)
	cfg, err := instance.LoadConfig(filepath.Join(root, instance.ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	schemaVersion := instance.InstanceSchemaVersionRunners
	cfg.SchemaVersion = &schemaVersion
	cfg.Engine = &instance.EngineConfig{
		HostPort:  instance.DefaultTemporalHostPort,
		Namespace: instance.DefaultTemporalNamespace,
		TaskQueue: instance.DefaultEngineTaskQueue,
	}
	cfg.Runners = []instance.RunnerEntry{
		{Name: "self", Host: instance.RunnerHostSelfName},
		{
			Name: "image-runner",
			Host: "ghcr.io/example/goobers-copilot:latest",
			Provides: instance.RunnerProvides{
				OS:        instance.RunnerOSLinux,
				Shell:     true,
				Harnesses: []string{string(apiv1.HarnessCopilot)},
			},
		},
	}
	if err := instance.WriteConfig(filepath.Join(root, instance.ConfigFileName), cfg); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "harness", "auth", "copilot", "login", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, `interactive login unavailable in image runner "image-runner"`) {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestHarnessAuthCopilotStatusPropagatesUnknown(t *testing.T) {
	withHarnessAdapter(t, func(apiv1.Harness, harness.EnvironmentConfig, map[string][]string, func(context.Context) (string, error)) (harness.Adapter, error) {
		return &authStatusAdapter{FakeAdapter: &harnesstest.FakeAdapter{}, err: errors.New("probe failed")}, nil
	})
	code, _, stderr := runArgs(t, "harness", "auth", "copilot", "status")
	if code != 1 || !strings.Contains(stderr, "probe failed") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestDoctorHarnessAuthExposesCredentialFreeState(t *testing.T) {
	const secret = "ghp_secret_value"
	withHarnessAdapter(t, func(apiv1.Harness, harness.EnvironmentConfig, map[string][]string, func(context.Context) (string, error)) (harness.Adapter, error) {
		return &authStatusAdapter{
			FakeAdapter: &harnesstest.FakeAdapter{AdapterName: "copilot-cli"},
			info: harness.AuthInfo{
				Status:      harness.AuthStatusSignedOut,
				Executable:  "launcher copilot",
				Version:     "copilot version 1.0.0",
				Runner:      "configured launcher",
				ProfileDir:  "C:\\profile",
				Remediation: "goobers harness auth copilot login",
			},
		}, nil
	})
	code, stdout, stderr := runArgs(t, "doctor", "--harness-auth")
	if code != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{"HARNESS copilot auth: signed-out", "executable: launcher copilot", "version: copilot version 1.0.0", "runner: configured launcher", "profile: C:\\profile"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
		t.Fatalf("doctor leaked credential text: stdout=%q stderr=%q", stdout, stderr)
	}
}
