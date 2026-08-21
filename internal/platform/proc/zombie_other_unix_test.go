//go:build unix && !linux

package proc

// isZombie is a linux-only refinement (see zombie_linux_test.go): darwin and
// other unix platforms have no /proc, so probeAlive keeps its pre-#3395
// signal-0-only behavior there — non-linux CI runners reap orphans promptly
// enough that this has not been observed to matter in practice.
func isZombie(int) bool {
	return false
}
