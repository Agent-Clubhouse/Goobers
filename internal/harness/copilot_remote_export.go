package harness

import (
	"bytes"
	"slices"
)

// copilotNoRemoteExportFlag stops the Copilot CLI from exporting the session
// to GitHub web and mobile (it also disables remote control). Goobers must not
// upload agent sessions by default (no-phone-home), so the adapter appends it
// to every Copilot session argv it builds — after any operator launcher
// override, ExtraArgs, or preflight arguments — rather than carrying it in the
// default launcher prefix, which a runner.harnessCommand override replaces.
//
// MEASURED: Copilot CLI 1.0.52 is the first release that accepts the flag;
// 1.0.51 and older reject it as "unknown option" before doing any work.
const copilotNoRemoteExportFlag = "--no-remote-export"

// copilotNoRemoteExportMinVersion is the oldest Copilot CLI that accepts
// copilotNoRemoteExportFlag.
const copilotNoRemoteExportMinVersion = "1.0.52"

// withCopilotNoRemoteExport returns argv with copilotNoRemoteExportFlag
// appended unless it is already present.
func withCopilotNoRemoteExport(argv []string) []string {
	if slices.Contains(argv, copilotNoRemoteExportFlag) {
		return argv
	}
	return append(argv, copilotNoRemoteExportFlag)
}

// copilotRemoteExportUnsupported reports whether a Copilot CLI invocation was
// refused because the installed CLI predates copilotNoRemoteExportFlag.
func copilotRemoteExportUnsupported(result ProcessResult) bool {
	marker := []byte("unknown option '" + copilotNoRemoteExportFlag + "'")
	return bytes.Contains(result.Transcript, marker) || bytes.Contains(result.Stderr, marker)
}

// copilotRemoteExportUpgradeHint is the actionable diagnostic for a CLI that
// rejects copilotNoRemoteExportFlag.
func copilotRemoteExportUpgradeHint() string {
	return "the installed Copilot CLI does not accept " + copilotNoRemoteExportFlag +
		", which Goobers passes to every Copilot session so agent sessions are never exported to GitHub; " +
		"upgrade the Copilot CLI to " + copilotNoRemoteExportMinVersion + " or newer"
}
