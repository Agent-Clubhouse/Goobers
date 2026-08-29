//go:build unix && !linux

package proc

// zombie is the non-linux fallback: darwin and the other unixes have no /proc
// state file to read, and the portable alternatives (sysctl KERN_PROC on
// darwin, parsing ps output elsewhere) are not worth their failure modes for a
// platform that is not the shipped container runtime. Reporting "not a zombie"
// keeps Alive exactly as it behaves today off linux — the fail-toward-alive
// direction doc.go requires.
func zombie(int) bool {
	return false
}
