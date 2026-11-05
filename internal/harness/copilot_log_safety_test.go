package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/diagnostictmp"
	"github.com/goobers/goobers/internal/journal"
)

func assertCopilotLogOutsideWorkspace(t *testing.T, workspace, logDir string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, logDir)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		t.Fatal("raw Copilot logs are inside the workspace")
	}
}

func setCopilotTempRoot(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, root)
	}
}

func TestCopilotRawLogsPrivateAndCleanedOnEveryExit(t *testing.T) {
	for _, sandboxed := range []bool{false, true} {
		for _, outcome := range []string{"success", "process failure", "cancellation", "sandbox failure"} {
			if outcome == "sandbox failure" && !sandboxed {
				continue
			}
			t.Run(outcome+"/sandbox="+map[bool]string{true: "true", false: "false"}[sandboxed], func(t *testing.T) {
				workspace, tempRoot := t.TempDir(), t.TempDir()
				setCopilotTempRoot(t, tempRoot)
				sb := &stubSandbox{}
				if outcome == "sandbox failure" {
					sb.wrapErr = errors.New("synthetic sandbox failure")
				}
				var logDir string
				runner := &fakeProcessRunner{act: func(req ProcessRequest) error {
					flag := slices.Index(req.Command, "--log-dir")
					if flag < 0 || flag+1 >= len(req.Command) {
						t.Fatal("invocation has no private log directory")
					}
					logDir = req.Command[flag+1]
					assertCopilotLogOutsideWorkspace(t, workspace, logDir)
					info, err := os.Stat(logDir)
					if err != nil {
						t.Fatal(err)
					}
					if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
						t.Fatal("raw log directory is not owner-private")
					}
					if err := os.WriteFile(filepath.Join(logDir, "process.log"), []byte("synthetic credential-bearing debug record"), 0o600); err != nil {
						t.Fatal(err)
					}
					switch outcome {
					case "process failure":
						return errors.New("synthetic process failure")
					case "cancellation":
						return context.Canceled
					default:
						return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
					}
				}}
				req := RunRequest{Workspace: workspace, Envelope: testEnvelope(workspace), CompletionPath: DefaultResultPath}
				if sandboxed {
					req.Sandbox = sb
				}
				adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}
				_, err := adapter.Run(context.Background(), req)
				if (err == nil) != (outcome == "success") {
					t.Fatalf("unexpected invocation outcome: %v", err)
				}
				if sandboxed && outcome != "sandbox failure" && (len(sb.policies) != 1 || !slices.Equal(sb.policies[0].WritableRoots, []string{logDir})) {
					t.Fatal("sandbox did not grant exactly the private log directory")
				}
				namespaces, err := os.ReadDir(tempRoot)
				if err != nil || len(namespaces) != 1 || !namespaces[0].IsDir() {
					t.Fatalf("unexpected diagnostics namespaces: count=%d err=%v", len(namespaces), err)
				}
				entries, err := os.ReadDir(filepath.Join(tempRoot, namespaces[0].Name()))
				if err != nil || len(entries) != 1 || entries[0].Name() != ".lock" {
					t.Fatalf("invocation retained private runtime files: count=%d err=%v", len(entries), err)
				}
				if dirExists(filepath.Join(workspace, ".goobers", "sandbox", "logs")) {
					t.Fatal("invocation created workspace debug logs")
				}
			})
		}
	}
}

func TestCopilotRawLogsRejectWorkspaceTempRoots(t *testing.T) {
	for _, mode := range []string{"workspace", "nested", "symlink", "workspace alias"} {
		t.Run(mode, func(t *testing.T) {
			workspace, outside := t.TempDir(), t.TempDir()
			parent := workspace
			if mode != "workspace" {
				parent = filepath.Join(workspace, "temp")
				if err := os.Mkdir(parent, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "symlink" || mode == "workspace alias" {
				link := filepath.Join(outside, "alias")
				target := parent
				if mode == "workspace alias" {
					target = workspace
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				if mode == "workspace alias" {
					workspace = link
				} else {
					parent = link
				}
			}
			setCopilotTempRoot(t, parent)
			dir, cleanup, err := prepareCopilotMCPLog(RunRequest{Workspace: workspace}, nil)
			defer cleanup()
			if err == nil || dir != "" || !strings.Contains(err.Error(), "outside the workspace") {
				t.Fatalf("workspace temp root was not rejected: %v", err)
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected temp root contains diagnostic files: count=%d err=%v", len(entries), err)
			}
		})
	}
}

func TestCopilotRawLogsRejectCallerLogDirectory(t *testing.T) {
	for _, args := range [][]string{{"--log-dir", ".goobers/raw-logs"}, {"--log-dir=.goobers/raw-logs"}} {
		workspace := t.TempDir()
		runner := &fakeProcessRunner{act: func(ProcessRequest) error {
			t.Fatal("invocation with a caller-controlled log directory was dispatched")
			return nil
		}}
		adapter := &CopilotAdapter{Command: []string{"copilot"}, ExtraArgs: args, Runner: runner}
		_, err := adapter.Run(context.Background(), RunRequest{Workspace: workspace, Envelope: testEnvelope(workspace)})
		if err == nil || !strings.Contains(err.Error(), "log directory is adapter-managed") {
			t.Fatalf("caller-controlled log directory was not rejected: %v", err)
		}
	}
}

func TestCopilotRawLogsActiveScopesSurviveForeignSweep(t *testing.T) {
	workspace, tempRoot := t.TempDir(), t.TempDir()
	setCopilotTempRoot(t, tempRoot)
	unrelated := filepath.Join(tempRoot, "unrelated-owner")
	if err := os.Mkdir(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}
	var active []string
	for range 3 {
		dir, cleanup, err := prepareCopilotMCPLog(RunRequest{Workspace: workspace}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cleanup)
		if err := os.WriteFile(filepath.Join(dir, "process.log"), []byte("synthetic raw log"), 0o600); err != nil {
			t.Fatal(err)
		}
		active = append(active, dir)
	}
	removed, err := diagnostictmp.Sweep(tempRoot)
	if err != nil || removed != 0 {
		t.Fatalf("foreign sweep removed %d live scopes: %v", removed, err)
	}
	for _, dir := range active {
		if !dirExists(dir) {
			t.Fatal("foreign sweep removed a live Copilot capture")
		}
	}
	if !dirExists(unrelated) {
		t.Fatal("startup orphan sweep removed an unrelated directory")
	}
}

func TestCopilotControlDiagnosticsRedactExactCredentials(t *testing.T) {
	for _, exitCode := range []int{0, 1} {
		t.Run(map[int]string{0: "runner error", 1: "stderr"}[exitCode], func(t *testing.T) {
			const githubToken = "synthetic-opaque-github-credential"
			var connectionToken string
			runner := &fakeProcessRunner{result: ProcessResult{ExitCode: exitCode}}
			runner.act = func(req ProcessRequest) error {
				for _, entry := range req.Env {
					if value, ok := strings.CutPrefix(entry, "COPILOT_CONNECTION_TOKEN="); ok {
						connectionToken = value
					}
				}
				if connectionToken == "" {
					t.Fatal("control process omitted connection credential")
				}
				data, err := json.Marshal(map[string]string{"GH_TOKEN": githubToken, "COPILOT_CONNECTION_TOKEN": connectionToken})
				if err != nil {
					t.Fatal(err)
				}
				runner.result.Stderr = data
				return errors.New(string(data))
			}
			_, _, err := startCopilotControlProcess(context.Background(), runner, ProcessRequest{
				Command: []string{"copilot", "-p=", "--session-id", "owned-session"},
				Env:     []string{"GH_TOKEN=" + githubToken},
			}, 1)
			if err == nil || !errors.Is(err, errRequiredMCPUnavailable) {
				t.Fatal("missing control startup diagnostic")
			}
			message := err.Error()
			if strings.Contains(message, githubToken) || strings.Contains(message, connectionToken) || !strings.Contains(message, journal.Redacted) {
				t.Fatal("control startup diagnostic exposed a credential")
			}
		})
	}
}

func TestCopilotMCPDiagnosticsNeverCopyRawCredentialRecords(t *testing.T) {
	const githubToken = "synthetic-github-credential-from-log"
	const connectionToken = "synthetic-connection-credential-from-log"
	dir := writeCopilotLog(t, copilotLogPolicyCheck,
		`[DEBUG] environment {"GH_TOKEN":"`+githubToken+`","COPILOT_CONNECTION_TOKEN":"`+connectionToken+`"}`,
		`[DEBUG] MCP server stderr {"server_name":"goobers-io","stderr":"`+githubToken+` `+connectionToken+`"}`,
	)
	failures := copilotMCPServerFailures(goobersIORequest(), dir)
	encoded, err := json.Marshal(failures)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 || failures[0].Status != copilotMCPStatusHandshakeIncomplete {
		t.Fatal("structured MCP status was not preserved")
	}
	if strings.Contains(string(encoded), githubToken) || strings.Contains(string(encoded), connectionToken) {
		t.Fatal("MCP diagnostic copied a raw credential record")
	}
}
