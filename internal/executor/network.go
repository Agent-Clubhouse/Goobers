package executor

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ProbeNoNetwork runs a no-op command through the same platform-specific
// network:none setup used by deterministic stages.
func ProbeNoNetwork(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "/bin/true")
	if _, err := configureCommandNetwork(cmd, apiv1.NetworkNone); err != nil {
		return err
	}
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if detail := strings.TrimSpace(string(output)); detail != "" {
		return fmt.Errorf("executor: network isolation probe: %w: %s", err, detail)
	}
	return fmt.Errorf("executor: network isolation probe: %w", err)
}

// configureCommandNetwork applies mode's platform-specific isolation to cmd.
// The returned marker is non-empty only when isolation was NOT actually
// applied (the Windows escape hatch fired, #2034) — every platform that
// genuinely isolates the command returns an empty marker. Callers that
// journal a stage's result must surface a non-empty marker somewhere visible
// beyond the child process's own env (see ShellExecutor.Run's
// Outputs["networkIsolation"]), so a host-global opt-out doesn't silently and
// invisibly de-isolate every later "isolated" stage.
func configureCommandNetwork(cmd *exec.Cmd, mode apiv1.NetworkMode) (marker string, err error) {
	switch mode {
	case "":
		return "", nil
	case apiv1.NetworkNone:
		return configureNoNetwork(cmd)
	default:
		return "", fmt.Errorf("executor: unknown network mode %q", mode)
	}
}

// describeNetworkNoneStartFailure enriches a network:none stage's fork/exec
// failure with an actionable diagnosis when the failure shape matches a
// known, platform-specific cause — currently just Linux's restricted-
// unprivileged-userns case (#4267), via networkNoneStartFailureHint's
// per-platform implementation. Any other failure (a missing binary, a
// permission problem unrelated to namespaces, ...) passes through unchanged.
func describeNetworkNoneStartFailure(mode apiv1.NetworkMode, err error) error {
	if mode != apiv1.NetworkNone || err == nil {
		return err
	}
	if hint := networkNoneStartFailureHint(err); hint != "" {
		return fmt.Errorf("%w: %s", err, hint)
	}
	return err
}
