package harness

import (
	"regexp"
	"slices"
)

// Copilot evaluates CLI denies before broad tool grants. Keep these on the
// final argv, including launcher overrides and resumed completion repairs.
// Prefix permissions are only a backstop; controlled sessions also inspect
// the complete command text, and OS confinement is the security boundary.
func withCopilotHostSafety(argv []string) []string {
	for _, command := range []string{
		"pkill", "killall",
		"/usr/bin/pkill", "/usr/bin/killall", "/bin/pkill", "/bin/killall",
		"/usr/bin/env pkill", "/usr/bin/env killall",
	} {
		flag := "--deny-tool=shell(" + command + ")"
		if !slices.Contains(argv, flag) {
			argv = append(argv, flag)
		}
	}
	return argv
}

var copilotNameKill = regexp.MustCompile(`\b(pkill|killall)\b`)
var copilotProcessLookup = regexp.MustCompile(`\bpgrep\b`)
var copilotSignalCommand = regexp.MustCompile(`\bkill\b`)

func copilotUnsafeShellCommand(command string) bool {
	return copilotNameKill.MatchString(command) ||
		(copilotProcessLookup.MatchString(command) && copilotSignalCommand.MatchString(command))
}
