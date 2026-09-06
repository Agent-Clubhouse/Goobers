//go:build darwin

package executor

import "os/exec"

const noNetworkSandboxProfile = `(version 1)(allow default)(deny network*)`

func configureNoNetwork(cmd *exec.Cmd) (marker string, err error) {
	targetPath := cmd.Path
	targetArgs := append([]string(nil), cmd.Args[1:]...)
	cmd.Path = "/usr/bin/sandbox-exec"
	cmd.Args = append([]string{"sandbox-exec", "-p", noNetworkSandboxProfile, targetPath}, targetArgs...)
	// Real isolation, always applied — no unsupported-isolation marker (#2034
	// is Windows-only; darwin never needs one).
	return "", nil
}

// networkNoneStartFailureHint: darwin's sandbox-exec isolation has no known
// EPERM-shaped capability gap analogous to Linux's restricted-userns case
// (#4267), so there is nothing more specific to add here.
func networkNoneStartFailureHint(error) string { return "" }
