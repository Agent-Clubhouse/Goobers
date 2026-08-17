package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/selfupdate"
)

func TestSelfUpdateCommandRoutesManualTarget(t *testing.T) {
	root := selfUpdateTestInstance(t, "20m")
	setSelfUpdateRepoRoute(t)
	resultFile := filepath.Join(t.TempDir(), "result.json")
	t.Setenv(executor.InputEnvVar("resultFile"), resultFile)

	var got selfupdate.PrepareOptions
	prepare := func(_ context.Context, opts selfupdate.PrepareOptions) (selfupdate.PrepareResult, error) {
		got = opts
		return selfupdate.PrepareResult{UpdateRequested: true, Policy: opts.Policy, Target: opts.Target}, nil
	}
	var stdout, stderr bytes.Buffer
	code := runSelfUpdateWith(
		[]string{"--policy", "manual", "--target", "v1.2.3", root},
		&stdout,
		&stderr,
		prepare,
	)

	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if got.Policy != selfupdate.PolicyManual || got.Target != "v1.2.3" {
		t.Fatalf("prepare options policy/target = %q/%q, want manual/v1.2.3", got.Policy, got.Target)
	}
	if !strings.Contains(stdout.String(), "target v1.2.3 staged") {
		t.Fatalf("stdout = %q, want staged target", stdout.String())
	}
}

func TestSelfUpdateCommandReportsAlreadyActiveResult(t *testing.T) {
	root := selfUpdateTestInstance(t, "20m")
	setSelfUpdateRepoRoute(t)
	resultFile := filepath.Join(t.TempDir(), "result.json")
	t.Setenv(executor.InputEnvVar("resultFile"), resultFile)

	prepare := func(_ context.Context, opts selfupdate.PrepareOptions) (selfupdate.PrepareResult, error) {
		return selfupdate.PrepareResult{Policy: opts.Policy, Target: "v1.2.3"}, nil
	}
	var stdout, stderr bytes.Buffer
	code := runSelfUpdateWith([]string{root}, &stdout, &stderr, prepare)

	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "target v1.2.3 is already active") {
		t.Fatalf("stdout = %q, want already-active message", stdout.String())
	}
	var result struct {
		UpdateRequested bool   `json:"updateRequested"`
		Policy          string `json:"policy"`
		Target          string `json:"target"`
	}
	raw, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.UpdateRequested || result.Policy != selfupdate.PolicyOnRelease || result.Target != "v1.2.3" {
		t.Fatalf("result = %+v", result)
	}
}

func TestSelfUpdateCommandRejectsInvalidPolicyTargets(t *testing.T) {
	root := selfUpdateTestInstance(t, "")
	setSelfUpdateRepoRoute(t)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "manual missing target", args: []string{"--policy", "manual", root}, want: "manual policy requires"},
		{name: "release with target", args: []string{"--target", "v1.2.3", root}, want: "on-release policy does not accept"},
		{name: "main with target", args: []string{"--policy", "on-main", "--target", "v1.2.3", root}, want: "on-main policy"},
		{name: "unknown policy", args: []string{"--policy", "nightly", root}, want: "unknown self-update policy"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runSelfUpdate(test.args, &stdout, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("code = %d, stderr = %q, want error containing %q", code, stderr.String(), test.want)
			}
		})
	}
}

func TestSelfUpdateCommandDerivesHealthWindowFromDaemonLiveness(t *testing.T) {
	root := selfUpdateTestInstance(t, "20m")
	setSelfUpdateRepoRoute(t)
	t.Setenv(executor.InputEnvVar("resultFile"), filepath.Join(t.TempDir(), "result.json"))

	var got selfupdate.PrepareOptions
	prepare := func(_ context.Context, opts selfupdate.PrepareOptions) (selfupdate.PrepareResult, error) {
		got = opts
		return selfupdate.PrepareResult{Policy: opts.Policy, Target: "v1"}, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runSelfUpdateWith([]string{"--health-ticks", "3", root}, &stdout, &stderr, prepare); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if got.HeartbeatInterval != 10*time.Minute {
		t.Fatalf("heartbeat interval = %s, want 10m", got.HeartbeatInterval)
	}
	if got.HealthTimeout != 40*time.Minute {
		t.Fatalf("health timeout = %s, want 40m", got.HealthTimeout)
	}
}

func TestResolveSelfUpdateEscalationToken(t *testing.T) {
	t.Setenv("ISSUE_WRITE_TOKEN", "issue-token")
	t.Setenv("REPO_TOKEN", "repo-token")
	repo := instance.RepoRef{
		Provider: "github",
		Owner:    "acme",
		Name:     "goobers",
		Token:    instance.TokenRef{Env: "REPO_TOKEN"},
	}
	cfg := &instance.Config{Credentials: []instance.CredentialGrant{{
		Capability: string(capability.GitHubIssuesWrite),
		Token:      instance.TokenRef{Env: "ISSUE_WRITE_TOKEN"},
	}}}

	token, err := resolveSelfUpdateEscalationToken(context.Background(), cfg, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "issue-token" {
		t.Fatalf("token = %q, want configured issue-write token", token)
	}
}

func TestResolveSelfUpdateEscalationTokenRejectsMissingOrInvalidCredential(t *testing.T) {
	repo := instance.RepoRef{Provider: "github", Owner: "acme", Name: "goobers"}
	tests := []struct {
		name  string
		token instance.TokenRef
		want  string
	}{
		{name: "missing", token: instance.TokenRef{Env: "UNSET_ISSUE_WRITE_TOKEN"}, want: "is not set"},
		{name: "invalid", token: instance.TokenRef{}, want: "must set exactly one"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &instance.Config{Credentials: []instance.CredentialGrant{{
				Capability: string(capability.GitHubIssuesWrite),
				Token:      test.token,
			}}}
			_, err := resolveSelfUpdateEscalationToken(context.Background(), cfg, repo, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestServiceSuperviseEntryPointExitCodes(t *testing.T) {
	root := selfUpdateTestInstance(t, "")
	supervisorErr := errors.New("daemon launch failed")
	tests := []struct {
		name       string
		supervise  func(context.Context, selfupdate.SupervisorOptions) error
		wantCode   int
		wantStderr string
	}{
		{
			name: "success",
			supervise: func(context.Context, selfupdate.SupervisorOptions) error {
				return nil
			},
			wantCode: 0,
		},
		{
			name: "supervisor error",
			supervise: func(context.Context, selfupdate.SupervisorOptions) error {
				return supervisorErr
			},
			wantCode:   1,
			wantStderr: supervisorErr.Error(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deps := serviceSuperviseDeps{
				runSupervisor:    test.supervise,
				isWindowsService: func() (bool, error) { return false, nil },
				runWindowsService: func(string, func(context.Context) int) (int, error) {
					t.Fatal("Windows service runner called outside Windows service")
					return 0, nil
				},
				setupSignalContext: func() (context.Context, func()) {
					ctx, cancel := context.WithCancel(context.Background())
					return ctx, cancel
				},
			}
			var stdout, stderr bytes.Buffer
			code := runServiceSuperviseWith([]string{root}, &stdout, &stderr, deps)
			if code != test.wantCode || !strings.Contains(stderr.String(), test.wantStderr) {
				t.Fatalf("code = %d, stderr = %q; want code %d and stderr containing %q",
					code, stderr.String(), test.wantCode, test.wantStderr)
			}
		})
	}

	code, _, stderr := runArgs(t, "__service-supervise", root, "extra")
	if code != 2 || !strings.Contains(stderr, "Usage: goobers __service-supervise") {
		t.Fatalf("hidden entry point usage: code = %d, stderr = %q", code, stderr)
	}
}

func selfUpdateTestInstance(t *testing.T, livenessTimeout string) string {
	t.Helper()
	root := t.TempDir()
	raw := "apiVersion: goobers.dev/v1alpha1\nkind: Instance\n"
	if livenessTimeout != "" {
		raw += "runner:\n  livenessTimeout: " + livenessTimeout + "\n"
	}
	if err := os.WriteFile(instance.NewLayout(root).ConfigFile(), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func setSelfUpdateRepoRoute(t *testing.T) {
	t.Helper()
	t.Setenv(executor.RepoProviderEnvVar, "github")
	t.Setenv(executor.RepoOwnerEnvVar, "acme")
	t.Setenv(executor.RepoNameEnvVar, "goobers")
}
