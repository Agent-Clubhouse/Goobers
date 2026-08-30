package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

func chooseGuidedRepositoryFolder(ctx context.Context) (string, bool, error) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		script := `[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new()
Add-Type -AssemblyName System.Windows.Forms
$dialog = New-Object System.Windows.Forms.FolderBrowserDialog
$dialog.Description = 'Choose a local Git repository'
$dialog.ShowNewFolderButton = $false
if ($dialog.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) {
  [Console]::Write($dialog.SelectedPath)
}`
		command = exec.CommandContext(
			ctx,
			"powershell.exe",
			"-NoProfile",
			"-NonInteractive",
			"-STA",
			"-Command",
			script,
		)
	case "darwin":
		command = exec.CommandContext(
			ctx,
			"osascript",
			"-e",
			`POSIX path of (choose folder with prompt "Choose a local Git repository")`,
		)
	case "linux":
		command = exec.CommandContext(
			ctx,
			"zenity",
			"--file-selection",
			"--directory",
			"--title=Choose a local Git repository",
		)
	default:
		return "", false, fmt.Errorf("folder browsing is not supported on %s; enter the path manually", runtime.GOOS)
	}

	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			if runtime.GOOS == "darwin" && strings.Contains(string(exitError.Stderr), "-128") {
				return "", true, nil
			}
			if runtime.GOOS == "linux" && exitError.ExitCode() == 1 {
				return "", true, nil
			}
		}
		return "", false, fmt.Errorf("open repository folder picker: %w", err)
	}
	path := strings.TrimSpace(string(output))
	if path == "" {
		return "", true, nil
	}
	return path, false, nil
}
