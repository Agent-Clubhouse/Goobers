//go:build linux

package testdep

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// userNamespaceProbeCommand is a no-op binary the probe execs purely to prove
// the clone that precedes it succeeded. It matches what internal/executor's own
// ProbeNoNetwork runs, so both capability probes behave identically on hosts
// that lack it: the exec fails with ENOENT, which is not permission-shaped and
// therefore deliberately does not count as a capability gap.
const userNamespaceProbeCommand = "/bin/true"

// probeUserNamespace forks a throwaway child into a new unprivileged user
// namespace, using the same single-ID uid/gid mapping the executor's isolation
// path applies (internal/executor/network_linux.go) so the probe exercises the
// mapping write too — bubblewrap performs the same steps. The clone runs in the
// child half of fork/exec, so a denial surfaces as a start error and the parent
// process keeps its own namespaces untouched.
func probeUserNamespace(ctx context.Context) error {
	uid, gid := os.Getuid(), os.Getgid()
	command := exec.CommandContext(ctx, userNamespaceProbeCommand)
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{{
			ContainerID: uid,
			HostID:      uid,
			Size:        1,
		}},
		GidMappings: []syscall.SysProcIDMap{{
			ContainerID: gid,
			HostID:      gid,
			Size:        1,
		}},
		GidMappingsEnableSetgroups: false,
	}
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	if detail := strings.TrimSpace(string(output)); detail != "" {
		return fmt.Errorf("testdep: user namespace probe: %w: %s", err, detail)
	}
	return fmt.Errorf("testdep: user namespace probe: %w", err)
}
