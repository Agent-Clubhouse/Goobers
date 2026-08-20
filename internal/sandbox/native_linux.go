//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const bubblewrapPreflightTimeout = 5 * time.Second

type nativeSandbox struct {
	bubblewrapPath string
}

func newNative() (Sandbox, error) {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("%w: bubblewrap (bwrap) not found on PATH", ErrUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), bubblewrapPreflightTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, path,
		"--die-with-parent",
		"--unshare-pid",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--", "/bin/true",
	).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			return nil, fmt.Errorf("%w: bubblewrap preflight: %w: %s", ErrUnavailable, err, detail)
		}
		return nil, fmt.Errorf("%w: bubblewrap preflight: %w", ErrUnavailable, err)
	}
	return nativeSandbox{bubblewrapPath: path}, nil
}

func (nativeSandbox) Mechanism() string { return "bwrap" }

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
