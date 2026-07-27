//go:build windows

package instance

import (
	"errors"

	"golang.org/x/sys/windows"
)

func isWindowsSymlinkPrivilegeError(err error) bool {
	return errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD)
}
