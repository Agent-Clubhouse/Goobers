//go:build linux

package executor

import (
	"os"
	"os/exec"
	"syscall"
)

func configureNoNetwork(cmd *exec.Cmd) error {
	uid := os.Getuid()
	gid := os.Getgid()
	// The one-ID user namespace lets a non-root daemon create the network
	// namespace without granting it any capability in the host namespaces.
	cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET
	cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{
		ContainerID: uid,
		HostID:      uid,
		Size:        1,
	}}
	cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{
		ContainerID: gid,
		HostID:      gid,
		Size:        1,
	}}
	cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	return nil
}
