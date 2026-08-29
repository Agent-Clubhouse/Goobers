package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

const windowsLargeRepoPreflightTimeout = 10 * time.Second

const windowsLargeRepoPreflightScript = `
$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath($env:GOOBERS_WORKCOPIES_ROOT).TrimEnd('\')
$devDrive = 'unknown'
try {
  $drive = [IO.Path]::GetPathRoot($root).TrimEnd('\').TrimEnd(':')
  $volume = Get-Volume -DriveLetter $drive -ErrorAction Stop
  if ($null -ne $volume.IsDevDrive) {
    $devDrive = if ($volume.IsDevDrive) { 'true' } else { 'false' }
  }
} catch {}
Write-Output "DEVDRIVE=$devDrive"

$defender = 'unknown'
try {
  $excluded = $false
  foreach ($entry in (Get-MpPreference -ErrorAction Stop).ExclusionPath) {
    $expanded = [Environment]::ExpandEnvironmentVariables($entry)
    if ($root -like $expanded) {
      $excluded = $true
      break
    }
    try {
      $path = [IO.Path]::GetFullPath($expanded).TrimEnd('\')
      if ($root -ieq $path -or $root.StartsWith($path + '\', [StringComparison]::OrdinalIgnoreCase)) {
        $excluded = $true
        break
      }
    } catch {}
  }
  $defender = if ($excluded) { 'true' } else { 'false' }
} catch {}
Write-Output "DEFENDER=$defender"
`

type windowsLargeRepoPreflightDeps struct {
	hostOS string
	probe  func(context.Context, string) ([]byte, error)
}

func realWindowsLargeRepoPreflightDeps() windowsLargeRepoPreflightDeps {
	return windowsLargeRepoPreflightDeps{
		hostOS: runtime.GOOS,
		probe: func(ctx context.Context, root string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", windowsLargeRepoPreflightScript)
			cmd.Env = append(os.Environ(), "GOOBERS_WORKCOPIES_ROOT="+root)
			return cmd.CombinedOutput()
		},
	}
}

func windowsLargeRepoEnvironmentWarning(cfg *instance.Config, workcopiesRoot string, deps windowsLargeRepoPreflightDeps) string {
	if deps.hostOS != "windows" || !hasLargeRepo(cfg) {
		return ""
	}
	root, err := filepath.Abs(workcopiesRoot)
	if err != nil {
		return fmt.Sprintf("warning: Windows large-repo preflight could not resolve the workcopies root %q: %v", workcopiesRoot, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), windowsLargeRepoPreflightTimeout)
	defer cancel()
	output, err := deps.probe(ctx, root)
	if err != nil {
		return fmt.Sprintf("warning: Windows large-repo preflight could not inspect Defender exclusions or the workcopies volume for %s: %v; see docs/guides/windows-large-repo-runbook.md", root, err)
	}
	status := parseWindowsLargeRepoPreflight(output)
	if status.devDrive == "true" || status.defender == "true" {
		return ""
	}
	if status.devDrive == "false" && status.defender == "false" {
		return fmt.Sprintf("warning: large-repo workcopies root %s is neither on a Windows Dev Drive nor excluded from Microsoft Defender scanning; small-file I/O may be severely degraded; see docs/guides/windows-large-repo-runbook.md", root)
	}
	return fmt.Sprintf("warning: Windows large-repo preflight could not verify that %s is on a Dev Drive or excluded from Defender scanning; see docs/guides/windows-large-repo-runbook.md", root)
}

func hasLargeRepo(cfg *instance.Config) bool {
	if cfg == nil {
		return false
	}
	for _, repo := range cfg.Repos {
		if repo.LargeRepo {
			return true
		}
	}
	return false
}

type windowsLargeRepoPreflightStatus struct {
	devDrive string
	defender string
}

func parseWindowsLargeRepoPreflight(output []byte) windowsLargeRepoPreflightStatus {
	var status windowsLargeRepoPreflightStatus
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "DEVDRIVE":
			status.devDrive = value
		case "DEFENDER":
			status.defender = value
		}
	}
	return status
}
