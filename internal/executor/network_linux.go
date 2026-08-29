//go:build linux

package executor

import (
	"os"
	"os/exec"
	"syscall"
)

func configureNoNetwork(cmd *exec.Cmd) (marker string, err error) {
	uid := os.Getuid()
	gid := os.Getgid()
	// Every live stage arrives here via proc.Configure first, which already
	// allocates SysProcAttr — but a caller that skips that (e.g.
	// ProbeNoNetwork's bare exec.Cmd, used by the WSL preflight and the
	// #3397 test capability probe) would otherwise nil-pointer-dereference
	// on the very next line. Match proc.Configure's own idiom so this
	// function is safe to call directly.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
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
	// Real isolation, always applied — no unsupported-isolation marker (#2034
	// is Windows-only; linux never needs one).
	return "", nil
}
