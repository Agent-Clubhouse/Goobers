package harness

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestCopilotPreflightProbesCompleteLauncherPrefix(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := []string{program, "wrapper-subcommand", "--profile", "fixture"}
	original := append([]string(nil), command...)
	var calls [][]string
	runner := &fakeProcessRunner{
		result: ProcessResult{Transcript: []byte("copilot fixture version")},
		act: func(req ProcessRequest) error {
			calls = append(calls, append([]string(nil), req.Command...))
			return nil
		},
	}
	adapter := &CopilotAdapter{Command: command, VersionArgs: []string{"version", "--short"}, AuthCheckArgs: []string{"auth", "status"}, Runner: runner}
	if _, err := adapter.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		append(append([]string(nil), resolveHarnessCommand(original)...), "version", "--short"),
		append(append([]string(nil), resolveHarnessCommand(original)...), "auth", "status"),
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("preflight checked a different launcher than dispatch: got %q, want %q", calls, want)
	}
	if !reflect.DeepEqual(command, original) {
		t.Fatalf("preflight mutated configured launcher: %q", command)
	}
}

func TestCopilotVersionPreflightRequiresBoundedStdoutNotDiagnostics(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, stdout string
		wantError    bool
	}{
		{name: "stderr only", wantError: true},
		{name: "stdout version", stdout: "fixture 1.2.3\n"},
		{name: "oversized stdout", stdout: strings.Repeat("x", int(maxPreflightDiagnosticBytes)+1), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &CopilotAdapter{Command: []string{program}, Runner: launcherProcessRunner(func(_ context.Context, req ProcessRequest) (ProcessResult, error) {
				_, err := io.WriteString(req.StdoutCapture, tc.stdout)
				return ProcessResult{Transcript: []byte("warning on stderr\n" + tc.stdout)}, err
			})}
			info, err := adapter.Preflight(context.Background())
			if (err != nil) != tc.wantError || !tc.wantError && info.Version != "fixture 1.2.3" {
				t.Fatalf("version=%q err=%v", info.Version, err)
			}
		})
	}
}

type launcherProcessRunner func(context.Context, ProcessRequest) (ProcessResult, error)

func (f launcherProcessRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	return f(ctx, req)
}

func TestLauncherContractRejectsAmbiguousSessionSemantics(t *testing.T) {
	for _, input := range []string{
		`{}`, `{"version":2,"sessionMode":"adapter-managed"}`,
		`{"version":1,"sessionMode":"auto"}`,
		`{"version":1,"sessionMode":"adapter-managed","sessionArgs":["--id"]}`,
		`{"version":1,"sessionMode":"templated"}`,
		`{"version":1,"sessionMode":"templated","sessionArgs":["--id","fixed"]}`,
		`{"version":1,"sessionMode":"templated","sessionArgs":["{sessionId}","{workspace}"]}`,
		`{"version":1,"sessionMode":"wrapper-managed","unknown":true}`,
		`{"version":1,"sessionMode":"wrapper-managed"} {}`,
	} {
		if _, err := parseLauncherContract([]byte(input)); err == nil {
			t.Errorf("accepted incompatible contract: %s", input)
		}
	}
}

func TestIncompatibleLauncherStopsBeforeAgentOrModelDiscovery(t *testing.T) {
	var calls int
	adapter := &CopilotAdapter{
		Command: []string{"wrapper", "copilot"}, RequireLauncherContract: true,
		Runner: launcherProcessRunner(func(_ context.Context, req ProcessRequest) (ProcessResult, error) {
			calls++
			if !reflect.DeepEqual(req.Command, []string{"wrapper", "copilot", launcherContractFlag}) {
				t.Fatalf("incompatible wrapper reached another probe or dispatch: %q", req.Command)
			}
			return ProcessResult{ExitCode: 2}, nil
		}),
	}
	if _, err := adapter.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("preflight = %v", err)
	}
	if _, err := adapter.ResolveConfig("auto", nil); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("admission = %v", err)
	}
	if calls != 2 {
		t.Fatalf("probes = %d, want preflight and admission only", calls)
	}
}

func TestLauncherPreflightRejectsConflictingConfiguredSessionSelector(t *testing.T) {
	adapter := &CopilotAdapter{
		Command: []string{"wrapper", "copilot", "--resume", "foreign-session"}, RequireLauncherContract: true,
		Runner: &fakeProcessRunner{act: func(req ProcessRequest) error {
			_, err := io.WriteString(req.StdoutCapture, `{"version":1,"sessionMode":"wrapper-managed"}`)
			return err
		}},
	}
	if _, err := adapter.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "selectors conflict") {
		t.Fatalf("conflict was not rejected before dispatch: %v", err)
	}
}

func TestLauncherSessionModesReachRunAndCaptureTheirOwnTranscript(t *testing.T) {
	for _, mode := range []string{"adapter-managed", "wrapper-managed", "templated"} {
		t.Run(mode, func(t *testing.T) {
			workspace, home := t.TempDir(), t.TempDir()
			t.Setenv("COPILOT_HOME", home)
			contract := launcherContract{Version: 1, SessionMode: mode}
			if mode == "templated" {
				contract.SessionArgs = []string{"--local-capture={sessionId}"}
			}
			data, err := json.Marshal(contract)
			if err != nil {
				t.Fatal(err)
			}
			var probes, attempts int
			var capturedPath string
			adapter := &CopilotAdapter{
				Command: []string{"wrapper", "copilot"}, RequireLauncherContract: true,
				ExtraEnvAllowlist: []string{"COPILOT_HOME"},
				Runner: launcherProcessRunner(func(_ context.Context, req ProcessRequest) (ProcessResult, error) {
					if req.Command[len(req.Command)-1] == launcherContractFlag {
						probes++
						if _, err := req.StdoutCapture.Write(data); err != nil {
							return ProcessResult{}, err
						}
						return ProcessResult{Transcript: data}, nil
					}
					attempts++
					switch mode {
					case "adapter-managed":
						id := commandOptionValue(req.Command, "--session-id")
						if id == "" {
							t.Fatal("adapter did not supply its ID")
						}
						capturedPath = copilotSessionLogPath(home, id)
					case "templated":
						for _, arg := range req.Command {
							if id, ok := strings.CutPrefix(arg, "--local-capture="); ok {
								capturedPath = copilotSessionLogPath(home, id)
							}
						}
					case "wrapper-managed":
						for _, entry := range req.Env {
							if path, ok := strings.CutPrefix(entry, "GOOBERS_SESSION_TRANSCRIPT="); ok {
								capturedPath = path
							}
						}
					}
					if mode != "adapter-managed" && copilotCommandSelectsSession(req.Command) {
						t.Fatal("adapter injected direct session semantics into another session mode")
					}
					if capturedPath == "" {
						t.Fatal("session capture path missing")
					}
					if err := os.MkdirAll(filepath.Dir(capturedPath), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(capturedPath, []byte(`{"type":"assistant.message","data":{"messageId":"result","content":"unique wrapper transcript"}}`+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					return ProcessResult{}, WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
				}),
			}
			out, err := adapter.Run(context.Background(), RunRequest{Workspace: workspace, Envelope: testEnvelope(workspace), CompletionPath: DefaultResultPath})
			if err != nil {
				t.Fatal(err)
			}
			if probes != 1 || attempts != 1 || !strings.Contains(string(out.Transcript), "unique wrapper transcript") {
				t.Fatalf("lost launcher contract/capture: probes=%d attempts=%d transcript=%s", probes, attempts, out.Transcript)
			}
			if mode == "wrapper-managed" {
				if _, err := os.Stat(capturedPath); !os.IsNotExist(err) {
					t.Fatalf("private export not cleaned up after capture: %v", err)
				}
			}
		})
	}
}
