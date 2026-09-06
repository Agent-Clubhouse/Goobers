//go:build !linux

package proc

import "os/exec"

// No per-child memory bound exists off Linux. macOS has no cgroups, and
// setrlimit's address-space bound is even less representative of RSS there
// than it is on Linux; Windows Job Objects could carry one but the daemon's
// co-tenancy incident (#4070) is a Linux-container problem and a Windows
// implementation with no incident behind it would be an unmeasured bound.
//
// The stage runs exactly as before, and Describe() says plainly that nothing
// is enforced rather than leaving a caller to assume otherwise.
func startBounded(cmd *exec.Cmd, _ uint64, _ bool) (*Tree, *MemoryBound, error) {
	return startUnbounded(cmd, "no per-child memory mechanism on this platform")
}
