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
	if err := configureCommandNetwork(cmd, apiv1.NetworkNone); err != nil {
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

func configureCommandNetwork(cmd *exec.Cmd, mode apiv1.NetworkMode) error {
	switch mode {
	case "":
		return nil
	case apiv1.NetworkNone:
		return configureNoNetwork(cmd)
	default:
		return fmt.Errorf("executor: unknown network mode %q", mode)
	}
}
