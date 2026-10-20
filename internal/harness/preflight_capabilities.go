package harness

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

const maxCapabilityHelpBytes int64 = 64 << 10

var codexRequiredPublicOptions = []string{"--json", "--ignore-rules", "--disable", "--sandbox", "--skip-git-repo-check", "--cd", "--config"}
var copilotRequiredPublicOptions = []string{"--allow-all-tools", "--log-level", "--log-dir", "--session-id", "--no-remote-export"}

// probeRequiredCLIFlags checks the documented options the adapter always passes,
// using bounded stdout from a read-only help request. This is deliberately not
// a version-number floor or an attestation of hidden RPCs, config-key semantics,
// valid credentials, or model entitlement. Copilot's working --headless option,
// for example, is absent from its public help and must not be rejected here.
func probeRequiredCLIFlags(ctx context.Context, runner ProcessRunner, command []string, harness string, args, env, required []string) (ReadinessCheck, error) {
	if !nativeHarnessCommand(command, harness) {
		return readinessCheck("capability_mismatch", "harness_cli_flags_unobservable", "unobservable", "custom command or launcher: native CLI option probe not applied; use the launcher's negotiated contract"), nil
	}
	bounded, cancel := context.WithTimeout(ctx, ReadinessTimeout)
	defer cancel()
	stdout := newTranscriptBuffer(maxCapabilityHelpBytes)
	env = overrideEnv(env, "NO_COLOR", "1")
	env = overrideEnv(env, "TERM", "dumb")
	result, err := runner.Run(bounded, ProcessRequest{Command: append(resolveHarnessCommand(command), args...), Env: env, MaxTranscriptBytes: maxCapabilityHelpBytes, StdoutCapture: stdout, Timeout: ReadinessTimeout})
	if err != nil || result.ExitCode != 0 {
		return ReadinessCheck{}, fmt.Errorf("harness: %s: cannot verify required CLI options with %s (exit %d): %w", harness, strings.Join(args, " "), result.ExitCode, capabilityProbeError(err))
	}
	if stdout.Truncated() {
		return ReadinessCheck{}, fmt.Errorf("harness: %s: CLI help stdout exceeded %d bytes; required options could not be verified", harness, maxCapabilityHelpBytes)
	}
	advertised := advertisedCLIOptions(string(stdout.Bytes()))
	var missing []string
	for _, option := range required {
		if !slices.Contains(advertised, option) {
			missing = append(missing, option)
		}
	}
	if len(missing) > 0 {
		return ReadinessCheck{}, fmt.Errorf("harness: %s: installed CLI help does not advertise required options %s; upgrade the CLI or configure a compatible launcher (a version string alone does not establish capability)", harness, strings.Join(missing, ", "))
	}
	return readinessCheck("capability_mismatch", "harness_cli_flags_supported", "supported", "required public CLI options advertised; hidden protocols, configuration semantics and model entitlement unverified"), nil
}

func capabilityProbeError(err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("help command failed")
}

func nativeHarnessCommand(command []string, harness string) bool {
	if len(command) != 1 {
		return false
	}
	name := strings.ToLower(filepath.Base(command[0]))
	for _, suffix := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		name = strings.TrimSuffix(name, suffix)
	}
	return name == harness
}

// Read option-definition lines, not prose/examples that happen to mention a
// flag. Both native CLIs print long options alone or after a short alias.
func advertisedCLIOptions(help string) []string {
	var options []string
	for line := range strings.SplitSeq(help, "\n") {
		fields := strings.Fields(line)
		for _, field := range fields {
			option := strings.TrimRight(field, ",=")
			if !strings.HasPrefix(option, "-") {
				break
			}
			if strings.HasPrefix(option, "--") {
				options = append(options, option)
			}
		}
	}
	return options
}
