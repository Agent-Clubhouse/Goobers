//go:build linux

package sandbox

import (
	"fmt"
	"os/exec"
)

type nativeSandbox struct {
	bubblewrapPath string
}

func newNative() (Sandbox, error) {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("%w: bubblewrap (bwrap) not found on PATH", ErrUnavailable)
	}
	return nativeSandbox{bubblewrapPath: path}, nil
}

func (s nativeSandbox) Wrap(command *exec.Cmd, policy Policy) error {
	validated, err := validate(command, policy)
	if err != nil {
		return err
	}
	targetPath := command.Path
	targetArgs := append([]string(nil), command.Args[1:]...)
	command.Path = s.bubblewrapPath
	args := []string{
		"bwrap",
		"--die-with-parent",
		"--unshare-pid",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--bind", validated.workspace, validated.workspace,
	}
	for _, root := range validated.writableRoots {
		args = append(args, "--bind", root, root)
	}
	args = append(args, "--chdir", command.Dir, "--", targetPath)
	command.Args = append(args, targetArgs...)
	return nil
}
