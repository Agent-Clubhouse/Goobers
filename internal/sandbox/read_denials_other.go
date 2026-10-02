//go:build !unix

package sandbox

import "os"

// No native read boundary exists on these platforms.
func singleLink(os.FileInfo) bool { return false }
