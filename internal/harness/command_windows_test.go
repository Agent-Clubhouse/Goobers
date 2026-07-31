package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		Env:     append(baseEnv(nil), "GOOBERS_PROMPT_CAPTURE="+outputPath),
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
		Env:     append(baseEnv(nil), "GOOBERS_PROMPT_CAPTURE="+outputPath),
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
