package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type readinessTestRunner struct {
	t        *testing.T
	calls    [][]string
	authErr  error
	authExit int
}

func (r *readinessTestRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > ReadinessTimeout {
		r.t.Fatal("probe has no bounded deadline")
	}
	if req.MaxTranscriptBytes != maxPreflightDiagnosticBytes {
		r.t.Fatal("unbounded transcript")
	}
	args := req.Command[1:]
	r.calls = append(r.calls, args)
	if reflect.DeepEqual(args, []string{"--version"}) {
		if req.StdoutCapture != nil {
			if _, err := req.StdoutCapture.Write([]byte("CLI 1.2.3 opaque-secret\n")); err != nil {
				r.t.Fatal(err)
			}
		}
		return ProcessResult{Transcript: []byte("CLI 1.2.3 opaque-secret\n")}, nil
	}
	if !reflect.DeepEqual(args, []string{"auth", "status"}) {
		r.t.Fatalf("unsafe probe %v", args)
	}
	return ProcessResult{Transcript: []byte("token=opaque-secret ghp_abcdefghijklmnopqrstuvwx123456789012"), ExitCode: r.authExit}, r.authErr
}
func readinessCode(report Readiness, code string) bool {
	for _, check := range report.Checks {
		if check.Code == code {
			return true
		}
	}
	return false
}
func TestReadinessSafePreflightAndRedaction(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		err  error
		exit int
		code string
	}{
		{"ready", nil, 0, "harness_authentication_ready"},
		{"auth", errors.New("opaque-secret"), 1, "harness_authentication_failed"},
		{"transport", ErrTimeout, 1, "harness_transport_failed"},
		{"cancel", context.Canceled, 1, "harness_probe_unobservable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := &readinessTestRunner{t: t, authErr: tt.err, authExit: tt.exit}
			adapter := &ClaudeAdapter{Command: []string{executable}, Runner: runner, ModelCredential: func(context.Context) (string, error) { t.Fatal("secret resolved"); return "", nil }}
			report := ProbeReadiness(context.Background(), adapter, apiv1.GooberSpec{}, false)
			if report.Version != "1.2.3" || !readinessCode(report, tt.code) || !readinessCode(report, "harness_headless_unobservable") {
				t.Fatalf("%+v", report)
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "opaque-secret") || strings.Contains(string(encoded), "ghp_") {
				t.Fatalf("leaked: %s", encoded)
			}
		})
	}
}
func TestReadinessConfiguredCredentialAndCopilotNeverPrompt(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, copilot := range []bool{false, true} {
		runner := &readinessTestRunner{t: t}
		var adapter Adapter = &ClaudeAdapter{Command: []string{executable}, Runner: runner}
		if copilot {
			adapter = &CopilotAdapter{Command: []string{executable}, Runner: runner, AuthCheckArgs: []string{"-p", "DO NOT EXECUTE"}}
		}
		report := ProbeReadiness(context.Background(), adapter, apiv1.GooberSpec{}, !copilot)
		if len(runner.calls) != 1 || !readinessCode(report, "harness_authentication_unobservable") {
			t.Fatalf("calls=%v report=%+v", runner.calls, report)
		}
	}
}
func TestReadinessConfigurationMismatchIsDistinct(t *testing.T) {
	check := CheckReadinessConfig(&ClaudeAdapter{}, apiv1.GooberSpec{Model: "not-a-model-opaque-secret"})
	if check.Code != "harness_configuration_mismatch" || strings.Contains(check.Detail, "opaque-secret") {
		t.Fatalf("%+v", check)
	}
}

type readinessCodexRunner struct {
	t        *testing.T
	commands [][]string
}

func (r *readinessCodexRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		r.t.Fatal("unbounded probe")
	}
	r.commands = append(r.commands, req.Command)
	if len(req.Command) < 3 || req.Command[1] != "login" || req.Command[2] != "status" {
		r.t.Fatalf("unexpected command %v", req.Command)
	}
	return ProcessResult{Transcript: []byte("Logged in using ChatGPT opaque-secret")}, nil
}
func TestReadinessCodexAmbientIgnoresConfiguredModelGrant(t *testing.T) {
	runner := &readinessCodexRunner{t: t}
	adapter := &CodexAdapter{Command: []string{"codex"}, Runner: runner, ModelCredential: func(context.Context) (string, error) { t.Fatal("resolved configured model token"); return "", nil }}
	spec := apiv1.GooberSpec{HarnessOptions: testHarnessOptions(t, map[string]interface{}{"auth": "ambient-chatgpt"})}
	check := readinessAuthentication(context.Background(), adapter, spec, true)
	if check.Code != "harness_authentication_ready" || len(runner.commands) != 1 {
		t.Fatalf("%+v commands=%v", check, runner.commands)
	}
	if !reflect.DeepEqual(runner.commands[0], []string{"codex", "login", "status", "-c", `cli_auth_credentials_store="keyring"`}) {
		t.Fatalf("%v", runner.commands)
	}
	if strings.Contains(check.Detail, "opaque-secret") {
		t.Fatal("secret leaked")
	}
}
