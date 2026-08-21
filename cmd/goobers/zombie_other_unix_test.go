//go:build unix && !linux

package main

// isZombie is a linux-only refinement (see zombie_linux_test.go): darwin and
// other unix platforms have no /proc, so waitForProcessGone and
// waitForProcessGroupGone keep their pre-#3395 signal-0-only behavior there.
func isZombie(int) bool {
	return false
}
