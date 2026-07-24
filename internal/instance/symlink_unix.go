//go:build !windows

package instance

func isWindowsSymlinkPrivilegeError(error) bool {
	return false
}
