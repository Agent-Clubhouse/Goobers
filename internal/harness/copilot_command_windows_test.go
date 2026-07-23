package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveCopilotCommandUsesPowerShellShim(t *testing.T) {
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

	got := resolveCopilotCommand([]string{"copilot", "--base-arg"})
	if len(got) < 9 || got[0] != "powershell.exe" {
		t.Fatalf("resolved command = %v, want PowerShell wrapper", got)
	}
	if got[7] != psPath || got[8] != "--base-arg" {
		t.Fatalf("resolved command = %v, want script %q and preserved args", got, psPath)
	}
}

func TestResolvedCopilotCommandPreservesMultilinePrompt(t *testing.T) {
	directory := t.TempDir()
	cmdPath := filepath.Join(directory, "copilot.cmd")
	psPath := filepath.Join(directory, "copilot.ps1")
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

	command := append(resolveCopilotCommand([]string{"copilot"}), "-p", prompt)
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
