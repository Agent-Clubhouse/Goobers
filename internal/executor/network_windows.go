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

// unsupportedNetworkIsolationMarker is the value configureNoNetwork returns
// (and the child env still carries, unchanged) whenever the escape hatch de-
// isolated this stage. It is also stamped into the stage's own ResultEnvelope
// Outputs (#2034) so the fact this stage ran unisolated is visible in the run
// journal/verdict and portal, not only in the child process's own env — an
// operator who set the host-global env var once to unblock one workflow can
// otherwise see nothing in the run record naming every later "isolated"
// stage that silently ran without it.
const unsupportedNetworkIsolationMarker = "unsupported-windows"

func configureNoNetwork(cmd *exec.Cmd) (marker string, err error) {
	if os.Getenv(allowUnisolatedNetworkNoneEnv) != "1" {
		return "", fmt.Errorf(
			"executor: network mode %q is unsupported on windows; set %s=1 only for trusted-local execution",
			"none",
			allowUnisolatedNetworkNoneEnv,
		)
	}
	cmd.Env = append(cmd.Env, "GOOBERS_NETWORK_ISOLATION="+unsupportedNetworkIsolationMarker)
	return unsupportedNetworkIsolationMarker, nil
}

// networkNoneStartFailureHint: Windows never reaches Start() with an
// isolation-shaped failure — configureNoNetwork above already refuses or
// degrades before Start() is ever called — so there is nothing more
// specific to add here. (#4267 is Linux-only; see network_linux.go.)
func networkNoneStartFailureHint(error) string { return "" }
