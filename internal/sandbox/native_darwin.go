//go:build darwin

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	seatbeltPath = "/usr/bin/sandbox-exec"
	seatbeltBase = `(version 1)
(allow default)
(deny file-write*)
(allow file-write*
    (subpath (param "WORKSPACE"))
    (literal "/dev/null")
    (literal "/dev/tty"))`
)

type nativeSandbox struct{}

func newNative() (Sandbox, error) {
	if _, err := os.Stat(seatbeltPath); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrUnavailable, seatbeltPath, err)
	}
	return nativeSandbox{}, nil
}

func (nativeSandbox) Wrap(command *exec.Cmd, policy Policy) error {
	validated, err := validate(command, policy)
	if err != nil {
		return err
	}
	targetPath := command.Path
	targetArgs := append([]string(nil), command.Args[1:]...)
	var profile strings.Builder
	profile.WriteString(seatbeltBase)
	args := []string{"sandbox-exec", "-D", "WORKSPACE=" + validated.workspace}
	for i, root := range validated.writableRoots {
		parameter := "WRITABLE_" + strconv.Itoa(i)
		fmt.Fprintf(&profile, "\n(allow file-write* (subpath (param %q)))", parameter)
		args = append(args, "-D", parameter+"="+root)
	}
	args = append(args, "-p", profile.String(), targetPath)
	command.Path = seatbeltPath
	command.Args = append(args, targetArgs...)
	return nil
}
