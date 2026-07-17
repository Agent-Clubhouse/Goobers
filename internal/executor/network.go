package executor

import (
	"fmt"
	"os/exec"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

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
