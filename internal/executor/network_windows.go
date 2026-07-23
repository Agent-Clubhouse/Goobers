package executor

import (
	"fmt"
	"os"
	"os/exec"
)

// Windows has no unprivileged equivalent to Linux network namespaces or
// macOS Seatbelt in Goobers. Fail closed unless the operator explicitly opts
// into trusted-local execution without network isolation.
const allowUnisolatedNetworkNoneEnv = "GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE"

func configureNoNetwork(cmd *exec.Cmd) error {
	if os.Getenv(allowUnisolatedNetworkNoneEnv) != "1" {
		return fmt.Errorf(
			"executor: network mode %q is unsupported on windows; set %s=1 only for trusted-local execution",
			"none",
			allowUnisolatedNetworkNoneEnv,
		)
	}
	cmd.Env = append(cmd.Env, "GOOBERS_NETWORK_ISOLATION=unsupported-windows")
	return nil
}
