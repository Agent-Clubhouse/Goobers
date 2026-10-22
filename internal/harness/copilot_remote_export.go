package harness

import (
	"bytes"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/version"
)

// copilotNoRemoteExportFlag stops the Copilot CLI from exporting the session
// to GitHub web and mobile (it also disables remote control). Goobers must not
// upload agent sessions by default (no-phone-home), so the adapter appends it
// to every Copilot session argv it builds — after any operator launcher
// override, ExtraArgs, or preflight arguments — rather than carrying it in the
// default launcher prefix, which a runner.harnessCommand override replaces.
// CLI 1.0.103 replaces this flag with --sync-session=off.
//
// MEASURED: Copilot CLI 1.0.52 is the first release that accepts the flag;
// 1.0.51 and older reject it as "unknown option" before doing any work.
const copilotNoRemoteExportFlag = "--no-remote-export"

// copilotNoRemoteExportMinVersion is the oldest Copilot CLI that accepts
// copilotNoRemoteExportFlag.
const copilotNoRemoteExportMinVersion = "1.0.52"

const copilotSyncSessionOffFlag = "--sync-session=off"

var copilotSyncSessionMinimum = version.MustParseSemantic("1.0.103")

func copilotSessionExportFlag(reported string) string {
	match := copilotUsageVersion.FindStringSubmatch(strings.TrimSpace(reported))
	if len(match) == 2 {
		parsed, err := version.ParseSemantic(strings.TrimRight(match[1], "."))
		if err == nil && !parsed.LessThan(copilotSyncSessionMinimum) {
			return copilotSyncSessionOffFlag
		}
	}
	return copilotNoRemoteExportFlag
}

// Replace configured export controls so obsolete flags cannot break the
// selected CLI and an operator's sync opt-in cannot override the opt-out.
func withCopilotNoRemoteExport(argv []string, reported string) []string {
	filtered := make([]string, 0, len(argv)+1)
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if i > 0 {
			if arg == "--sync-session" {
				if i+1 < len(argv) && (argv[i+1] == "on" || argv[i+1] == "off") {
					i++
				}
				continue
			}
			if arg == copilotNoRemoteExportFlag || arg == "--remote-export" || strings.HasPrefix(arg, "--sync-session=") {
				continue
			}
		}
		filtered = append(filtered, arg)
	}
	return append(filtered, copilotSessionExportFlag(reported))
}

// copilotRemoteExportUnsupported reports whether a Copilot CLI invocation was
// refused because the installed CLI or launcher rejects the selected opt-out.
func copilotRemoteExportUnsupported(result ProcessResult) bool {
	for _, flag := range []string{copilotNoRemoteExportFlag, "--sync-session"} {
		marker := []byte("unknown option '" + flag)
		if bytes.Contains(result.Transcript, marker) || bytes.Contains(result.Stderr, marker) {
			return true
		}
	}
	return false
}

// copilotRemoteExportUnsupportedError fails preflight for an incompatible
// CLI or launcher. Launching without the flag is not a safe
// fallback: those releases already export sessions to GitHub (Mission
// Control / cross-device session sync) when the user's Copilot configuration
// enables it, and offer no per-invocation switch to turn export off.
func copilotRemoteExportUnsupportedError(version string) error {
	if version == "" {
		version = "(unknown version)"
	}
	return fmt.Errorf("harness: copilot-cli: %s rejected the session export opt-out %s: Goobers requires Copilot CLI %s or newer "+
		"with a compatible session export control; upgrade the Copilot CLI (for example `copilot update`) "+
		"or update the forwarding launcher and restart; refusing to run with session export enabled",
		version, copilotSessionExportFlag(version), copilotNoRemoteExportMinVersion)
}
