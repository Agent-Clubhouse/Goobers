package avexclusion

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DefenderQueryTimeout bounds one exclusion-list read. PowerShell start-up
// plus Get-MpPreference is a second or two on a warm host; the bound exists
// so a hung Defender service can never hold a stage or a daemon start.
const DefenderQueryTimeout = 10 * time.Second

// StagePodQueryTimeout is the tighter bound the stage pod reads under. A
// pod pays this cost on EVERY stage attempt, and on the Server Core images
// this feature anticipates (PowerShell present, Defender cmdlet absent) it
// pays it for an answer that is always unknown — so the pod's bound is half
// the daemon's, which still clears a cold PowerShell start with room while
// halving the worst case. Overrunning it costs the same reported unknown
// those images produce anyway; the stage is never failed either way.
const StagePodQueryTimeout = 5 * time.Second

// defenderExclusionScript prints Microsoft Defender's path exclusions one
// per line with environment variables expanded — the read half of the
// existing large-repo preflight's probe (cmd/goobers/windowslargerepopreflight.go),
// without the matching, which lives in Go here so it is testable off
// Windows. Read-only by construction: Get-MpPreference never mutates.
const defenderExclusionScript = `
$ErrorActionPreference = 'Stop'
foreach ($entry in (Get-MpPreference -ErrorAction Stop).ExclusionPath) {
  Write-Output ([Environment]::ExpandEnvironmentVariables($entry))
}
`

// maxDefenderErrorOutput bounds how much of a failed probe's output rides
// the error, so a PowerShell stack trace does not become the advisory line.
const maxDefenderErrorOutput = 300

// Querier reads the host's AV path-exclusion list. The Windows
// implementation is QueryDefender; tests and non-Windows hosts substitute.
type Querier func(ctx context.Context) ([]string, error)

// QueryDefender reads Microsoft Defender's ExclusionPath list through
// PowerShell. It is only meaningful on a Windows host with Defender present;
// anywhere else it returns an error naming why (no powershell.exe, no
// Get-MpPreference cmdlet — Server Core container images ship PowerShell
// but may not ship Defender), which the caller reports as CoverageUnknown.
func QueryDefender(ctx context.Context) ([]string, error) {
	return QueryDefenderWithin(ctx, DefenderQueryTimeout)
}

// QueryDefenderStagePod is QueryDefender under StagePodQueryTimeout — the
// reader `goobers __dispatch-exec` uses, so a per-stage advisory cannot
// spend the daemon's ten-second budget on every attempt.
func QueryDefenderStagePod(ctx context.Context) ([]string, error) {
	return QueryDefenderWithin(ctx, StagePodQueryTimeout)
}

// QueryDefenderWithin is QueryDefender under an explicit bound, so the
// timeout a caller chose is also the timeout its error names.
func QueryDefenderWithin(ctx context.Context, bound time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", defenderExclusionScript)
	output, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if len(detail) > maxDefenderErrorOutput {
			detail = detail[:maxDefenderErrorOutput] + "…"
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("Get-MpPreference timed out after %s", bound)
		}
		if detail == "" {
			return nil, fmt.Errorf("Get-MpPreference: %w", err)
		}
		return nil, fmt.Errorf("Get-MpPreference: %w: %s", err, detail)
	}
	return ParseExclusionList(output), nil
}

// ParseExclusionList splits probe output into entries: one per line,
// trimmed, blanks dropped.
func ParseExclusionList(output []byte) []string {
	var entries []string
	for _, line := range strings.Split(string(output), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			entries = append(entries, line)
		}
	}
	return entries
}
