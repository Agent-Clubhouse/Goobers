package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

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
