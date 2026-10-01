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
	info harness.AuthInfo
	err  error
}

func (a *authStatusAdapter) AuthStatus(context.Context) (harness.AuthInfo, error) {
	return a.info, a.err
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
	runCopilotNativeAuthCommand = func(_ context.Context, command []string, _ harness.EnvironmentConfig, _ io.Writer, _ io.Writer) error {
		got = slices.Clone(command)
		return nil
	}
	code, stdout, stderr := runArgs(t, "harness", "auth", "copilot", "login")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !slices.Equal(got, []string{"copilot", "login"}) {
		t.Fatalf("native auth command = %v, want [copilot login]", got)
	}
}

func TestHarnessAuthCopilotLogoutFailsClearly(t *testing.T) {
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
