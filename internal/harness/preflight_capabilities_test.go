package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestRequiredPublicCLIOptionsUseHelpEvidenceNotVersion(t *testing.T) {
	for _, name := range []string{"codex", "copilot"} {
		t.Run(name, func(t *testing.T) {
			help := string(readTestData(t, name+"-capability-help.txt"))
			for _, missing := range []bool{false, true} {
				t.Run(map[bool]string{false: "advertised", true: "missing"}[missing], func(t *testing.T) {
					actual := help
					missingFlag := "--json"
					if name == "copilot" {
						missingFlag = "--no-remote-export"
					}
					if missing {
						actual = strings.ReplaceAll(actual, "      "+missingFlag+"\n", "")
						actual += "Description: use " + missingFlag + " if supported.\n"
					}
					version := "0.0.1" // capability evidence wins over an arbitrary version floor
					if missing {
						version = "99.0.0"
					}
					var commands [][]string
					runner := launcherProcessRunner(func(_ context.Context, req ProcessRequest) (ProcessResult, error) {
						commands = append(commands, req.Command)
						if slices.Contains(req.Command, "--help") {
							if len(req.Stdin) > 0 {
								t.Fatal("help probe sent a prompt")
							}
							if slices.Contains(req.Env, "OPENAI_API_KEY=sk-invalid-but-present") {
								t.Fatal("model credential leaked into capability probe")
							}
							if req.StdoutCapture == nil {
								t.Fatal("capability probe did not capture stdout separately")
							}
							_, _ = req.StdoutCapture.Write([]byte(actual))
							return ProcessResult{Transcript: []byte("stderr must not prove any CLI flag")}, nil
						}
						if !slices.Contains(req.Command, "--version") {
							t.Fatalf("unexpected model/auth operation: %v", req.Command)
						}
						if req.StdoutCapture != nil {
							_, _ = req.StdoutCapture.Write([]byte(name + " " + version + "\n"))
						}
						return ProcessResult{Transcript: []byte(name + " " + version + "\n")}, nil
					})
					command := []string{fixtureNativeCLI(t, name)}
					var adapter Adapter
					if name == "codex" {
						adapter = &CodexAdapter{Command: command, Runner: runner, ModelCredential: func(context.Context) (string, error) { return "sk-invalid-but-present", nil }}
					} else {
						adapter = &CopilotAdapter{Command: command, Runner: runner}
					}
					info, err := adapter.Preflight(context.Background())
					if missing {
						if err == nil || !strings.Contains(err.Error(), missingFlag) || !strings.Contains(err.Error(), "upgrade") {
							t.Fatalf("missing flag not actionable: %v", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if len(commands) != 2 {
						t.Fatalf("expected version/help only, got %v", commands)
					}
					if len(info.Checks) != 2 || info.Checks[0].Outcome != "supported" || info.Checks[1].Outcome != "unobservable" {
						t.Fatalf("capability/auth evidence conflated: %+v", info.Checks)
					}
				})
			}
		})
	}
}

func TestRequiredCLIOptionsCustomCommandsAreUnobservable(t *testing.T) {
	for _, command := range [][]string{{"wrapper"}, {"wrapper", "codex"}, {"codex", "--profile", "custom"}, nil} {
		calls := 0
		runner := launcherProcessRunner(func(context.Context, ProcessRequest) (ProcessResult, error) { calls++; return ProcessResult{}, nil })
		check, err := probeRequiredCLIFlags(context.Background(), runner, command, "codex", []string{"exec", "--help"}, nil, codexRequiredPublicOptions)
		if err != nil || calls != 0 || check.Outcome != "unobservable" {
			t.Fatalf("custom command inferred native support: %+v %v calls=%d", check, err, calls)
		}
	}
}

func TestRequiredCLIOptionsRejectUnreliableHelp(t *testing.T) {
	for _, test := range []struct {
		name, stdout string
		exit         int
		err          error
	}{
		{name: "stderr-only"},
		{name: "truncated", stdout: strings.Repeat("x", int(maxCapabilityHelpBytes)+1)},
		{name: "nonzero", stdout: "--json\n", exit: 2},
		{name: "deadline", err: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := launcherProcessRunner(func(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("unbounded capability check")
				}
				_, _ = req.StdoutCapture.Write([]byte(test.stdout))
				return ProcessResult{ExitCode: test.exit, Transcript: []byte("--json\n private-diagnostic-canary")}, test.err
			})
			_, err := probeRequiredCLIFlags(context.Background(), runner, []string{"codex"}, "codex", []string{"exec", "--help"}, nil, []string{"--json"})
			if err == nil || strings.Contains(err.Error(), "private-diagnostic-canary") {
				t.Fatalf("unreliable help accepted or diagnostic leaked: %v", err)
			}
			if test.err != nil && !errors.Is(err, test.err) {
				t.Fatal("transport error classification lost")
			}
		})
	}
}

func TestNativeCLICommandRecognizesWindowsShims(t *testing.T) {
	for _, name := range []string{"copilot", "copilot.exe", "copilot.cmd", "copilot.bat", "copilot.ps1"} {
		if !nativeHarnessCommand([]string{filepath.Join("bin", name)}, "copilot") {
			t.Fatalf("native shim rejected: %s", name)
		}
	}
}

func fixtureNativeCLI(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("fixture executable; ProcessRunner intercepts execution"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCopilotAuthStatusRequiresAuthenticationEvidence(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		t.Run(map[bool]string{false: "version-only", true: "auth-probe"}[authenticated], func(t *testing.T) {
			adapter := &CopilotAdapter{Command: []string{fixtureNativeCLI(t, "copilot")}}
			if authenticated {
				adapter.AuthCheckArgs = []string{"auth-fixture"}
			}
			adapter.Runner = launcherProcessRunner(func(_ context.Context, req ProcessRequest) (ProcessResult, error) {
				output := []byte("copilot 1.0.100\n")
				if slices.Contains(req.Command, "--help") {
					output = readTestData(t, "copilot-capability-help.txt")
				}
				if req.StdoutCapture != nil {
					_, _ = req.StdoutCapture.Write(output)
				}
				return ProcessResult{Transcript: output}, nil
			})
			info, err := adapter.AuthStatus(context.Background())
			want := AuthStatusUnknown
			if authenticated {
				want = AuthStatusAuthenticated
			}
			if err != nil || info.Status != want {
				t.Fatalf("status=%+v err=%v want=%s", info, err, want)
			}
		})
	}
}

func TestCodexAmbientCapabilitiesRequireIgnoreUserConfig(t *testing.T) {
	for _, supported := range []bool{false, true} {
		help := string(readTestData(t, "codex-capability-help.txt"))
		if !supported {
			help = strings.ReplaceAll(help, "      --ignore-user-config\n", "")
		}
		loginCalls := 0
		adapter := &CodexAdapter{Command: []string{fixtureNativeCLI(t, "codex")}, Runner: launcherProcessRunner(func(_ context.Context, req ProcessRequest) (ProcessResult, error) {
			output := "codex 0.0.1\n"
			if slices.Contains(req.Command, "--help") {
				output = help
			}
			if slices.Contains(req.Command, "login") {
				loginCalls++
				output = "Logged in using ChatGPT\n"
			}
			if req.StdoutCapture != nil {
				_, _ = req.StdoutCapture.Write([]byte(output))
			}
			return ProcessResult{Transcript: []byte(output)}, nil
		})}
		info, err := adapter.PreflightConfig(context.Background(), "", testHarnessOptions(t, map[string]interface{}{"auth": "ambient-chatgpt"}))
		if supported {
			if err != nil || loginCalls != 1 || info.Checks[1].Outcome != "supported" {
				t.Fatalf("info=%+v login=%d err=%v", info, loginCalls, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "--ignore-user-config") || loginCalls != 0 {
			t.Fatalf("missing option: login=%d err=%v", loginCalls, err)
		}
	}
}
