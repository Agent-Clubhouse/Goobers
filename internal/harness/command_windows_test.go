package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestResolveHarnessCommandUsesPowerShellShim(t *testing.T) {
	directory := t.TempDir()
	cmdPath := filepath.Join(directory, "claude.cmd")
	psPath := filepath.Join(directory, "claude.ps1")
	if err := os.WriteFile(cmdPath, []byte("@echo off\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(psPath, []byte("exit 0\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")

	got := resolveHarnessCommand([]string{"claude", "--base-arg"})
	if len(got) < 9 || got[0] != "powershell.exe" {
		t.Fatalf("resolved command = %v, want PowerShell wrapper", got)
	}
	if got[7] != psPath || got[8] != "--base-arg" {
		t.Fatalf("resolved command = %v, want script %q and preserved args", got, psPath)
	}
}

func TestResolveStdioHarnessCommandKeepsTheNativeShim(t *testing.T) {
	// Regression: a stdio JSON-RPC connection must NOT go through npm's .ps1
	// shim. That shim forwards a piped stdin through PowerShell's $input
	// enumerator, which is line-oriented and buffered, so the handshake never
	// completes and the caller blocks until its deadline. Model discovery hung
	// for its full timeout, once per goober, before this split existed.
	directory := t.TempDir()
	cmdPath := filepath.Join(directory, "copilot.cmd")
	psPath := filepath.Join(directory, "copilot.ps1")
	if err := os.WriteFile(cmdPath, []byte("@echo off\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(psPath, []byte("exit 0\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")

	got := resolveStdioHarnessCommand([]string{"copilot", "--stdio"})
	if len(got) != 2 {
		t.Fatalf("resolved command = %v, want the shim plus its argument", got)
	}
	if got[0] != cmdPath {
		t.Errorf("resolved command[0] = %q, want the native shim %q", got[0], cmdPath)
	}
	if strings.EqualFold(filepath.Ext(got[0]), ".ps1") || strings.Contains(strings.ToLower(got[0]), "powershell") {
		t.Errorf("resolved command = %v, want no PowerShell wrapper for a stdio connection", got)
	}
	if got[1] != "--stdio" {
		t.Errorf("resolved command = %v, want preserved arguments", got)
	}
}

func TestResolvedHarnessCommandPreservesMultilinePrompt(t *testing.T) {
	directory := t.TempDir()
	cmdPath := filepath.Join(directory, "claude.cmd")
	psPath := filepath.Join(directory, "claude.ps1")
	outputPath := filepath.Join(directory, "prompt.txt")
	if err := os.WriteFile(cmdPath, []byte("@echo off\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := `param([string]$Flag, [string]$Prompt)
[System.IO.File]::WriteAllText($env:GOOBERS_PROMPT_CAPTURE, $Prompt, [System.Text.UTF8Encoding]::new($false))
`
	if err := os.WriteFile(psPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	t.Setenv("GOOBERS_PROMPT_CAPTURE", outputPath)
	prompt := "---\nrole: curator\n---\n## Task\nExecute now."

	command := append(resolveHarnessCommand([]string{"claude"}), "-p", prompt)
	result, err := (ExecProcessRunner{}).Run(t.Context(), ProcessRequest{
		Command: command,
		Env:     append(baseEnv(nil, nil), "GOOBERS_PROMPT_CAPTURE="+outputPath),
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("run resolved command: result=%+v err=%v transcript=%s", result, err, result.Transcript)
	}
	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(string(got), "\r\n", "\n") != prompt {
		t.Fatalf("captured prompt = %q, want %q", got, prompt)
	}
}

// TestResolvedCopilotCommandPreservesBackticksInPrompt verifies that backtick
// characters (PowerShell's escape prefix) survive the -File shim unchanged.
// Without this, "`n" in label names like `goobers:needs-human` would be
// converted to a newline by PowerShell's double-quote expansion, corrupting
// the curator instructions.
func TestResolvedHarnessCommandPreservesBackticksInPrompt(t *testing.T) {
	directory := t.TempDir()
	cmdPath := filepath.Join(directory, "claude.cmd")
	psPath := filepath.Join(directory, "claude.ps1")
	outputPath := filepath.Join(directory, "prompt.txt")
	if err := os.WriteFile(cmdPath, []byte("@echo off\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := `param([string]$Flag, [string]$Prompt)
[System.IO.File]::WriteAllText($env:GOOBERS_PROMPT_CAPTURE, $Prompt, [System.Text.UTF8Encoding]::new($false))
`
	if err := os.WriteFile(psPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	t.Setenv("GOOBERS_PROMPT_CAPTURE", outputPath)
	// Include backtick sequences that PowerShell would expand in double-quoted
	// strings: `n (newline), `t (tab), `r (CR), `goobers:needs-human` (label
	// name starting with `n = newline escape).
	prompt := "remove the `goobers:needs-human` label\nadd `goobers:ready` directly"

	command := append(resolveHarnessCommand([]string{"claude"}), "-p", prompt)
	result, err := (ExecProcessRunner{}).Run(t.Context(), ProcessRequest{
		Command: command,
		Env:     append(baseEnv(nil, nil), "GOOBERS_PROMPT_CAPTURE="+outputPath),
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("run resolved command: result=%+v err=%v transcript=%s", result, err, result.Transcript)
	}
	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(string(got), "\r\n", "\n") != prompt {
		t.Fatalf("captured prompt = %q, want %q (backticks must survive the PowerShell shim unchanged)", got, prompt)
	}
}

func TestCopilotWindowsShimQuotedPromptUsesStdin(t *testing.T) {
	directory := t.TempDir()
	cmdPath := filepath.Join(directory, "copilot.cmd")
	psPath := filepath.Join(directory, "copilot.ps1")
	if err := os.WriteFile(cmdPath, []byte("@echo off\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(psPath, []byte("exit 0\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")

	workspace := t.TempDir()
	var calls []ProcessRequest
	runner := &fakeProcessRunner{result: ProcessResult{Transcript: []byte("finished"), ExitCode: 0}}
	runner.act = func(req ProcessRequest) error {
		calls = append(calls, req)
		if len(calls) == 2 {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "ok"})
		}
		return nil
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}
	_, err := adapter.Run(context.Background(), RunRequest{
		Mode:           ModeInvoke,
		Envelope:       testEnvelope(workspace),
		Instructions:   `Use the "quoted" title exactly.`,
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Timeout:        time.Minute,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("process calls = %d, want initial call plus one recovery", len(calls))
	}
	for i, call := range calls {
		if call.Command[0] != cmdPath {
			t.Fatalf("call %d command[0] = %q, want npm cmd shim %q", i, call.Command[0], cmdPath)
		}
		if slices.Contains(call.Command, psPath) || strings.Contains(strings.ToLower(strings.Join(call.Command, " ")), "powershell") {
			t.Fatalf("call %d used PowerShell for quoted prompt: %v", i, call.Command)
		}
		if !slices.Contains(call.Command, defaultPromptFlag+"=") {
			t.Fatalf("call %d missing empty prompt-mode flag: %v", i, call.Command)
		}
		for _, arg := range call.Command {
			if arg != defaultPromptFlag+"=" && (strings.HasPrefix(arg, defaultPromptFlag+"=") || strings.Contains(arg, `"quoted"`)) {
				t.Fatalf("call %d leaked prompt into argv: %v", i, call.Command)
			}
		}
		if !strings.Contains(string(call.Stdin), `"`) {
			t.Fatalf("call %d stdin missing quoted content: %q", i, call.Stdin)
		}
	}
	if !strings.Contains(string(calls[0].Stdin), `"quoted"`) {
		t.Fatalf("initial stdin missing quoted prompt: %q", calls[0].Stdin)
	}
	if !strings.Contains(string(calls[1].Stdin), "ended without writing the mandatory completion file") {
		t.Fatalf("recovery stdin missing completion repair prompt: %q", calls[1].Stdin)
	}
}

func TestCopilotWindowsCustomBatchLauncherKeepsPromptArgv(t *testing.T) {
	directory := t.TempDir()
	launcherPath := filepath.Join(directory, "copilot-launcher.cmd")
	if err := os.WriteFile(launcherPath, []byte("@echo off\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{Transcript: []byte("finished"), ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "ok"})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot-launcher"}, Runner: runner}
	_, err := adapter.Run(context.Background(), RunRequest{
		Mode:           ModeInvoke,
		Envelope:       testEnvelope(workspace),
		Instructions:   `Use the "quoted" title exactly.`,
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Timeout:        time.Minute,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.lastReq.Stdin) != 0 {
		t.Fatalf("custom launcher received stdin prompt unexpectedly: %q", runner.lastReq.Stdin)
	}
	prompt, ok := copilotPromptArgValue(runner.lastReq.Command)
	if !ok || !strings.Contains(prompt, `"quoted"`) {
		t.Fatalf("custom launcher prompt arg = %q, %v; command=%v", prompt, ok, runner.lastReq.Command)
	}
}
