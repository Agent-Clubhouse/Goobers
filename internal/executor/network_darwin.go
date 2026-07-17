//go:build darwin

package executor

import "os/exec"

const noNetworkSandboxProfile = `(version 1)(allow default)(deny network*)`

func configureNoNetwork(cmd *exec.Cmd) error {
	targetPath := cmd.Path
	targetArgs := append([]string(nil), cmd.Args[1:]...)
	cmd.Path = "/usr/bin/sandbox-exec"
	cmd.Args = append([]string{"sandbox-exec", "-p", noNetworkSandboxProfile, targetPath}, targetArgs...)
	return nil
}
