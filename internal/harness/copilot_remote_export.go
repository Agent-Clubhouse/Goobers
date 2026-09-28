package harness

import (
	"bytes"
	"fmt"
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

// copilotRemoteExportUnsupportedError fails preflight for a CLI that predates
// copilotNoRemoteExportFlag. Launching without the flag is not a safe
// fallback: those releases already export sessions to GitHub (Mission
// Control / cross-device session sync) when the user's Copilot configuration
// enables it, and offer no per-invocation switch to turn export off.
func copilotRemoteExportUnsupportedError(version string) error {
	if version == "" {
		version = "(unknown version)"
	}
	return fmt.Errorf("harness: copilot-cli: %s is too old: Goobers requires Copilot CLI %s or newer so it can "+
		"disable session export with %s; upgrade the Copilot CLI (for example `copilot update`) and restart",
		version, copilotNoRemoteExportMinVersion, copilotNoRemoteExportFlag)
}
