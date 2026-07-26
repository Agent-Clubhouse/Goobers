//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

func dashboardAddressInUse(err error) bool {
	// Windows uses WSAEACCES for excluded port ranges; --port=auto must skip
	// those unavailable ports just as it skips ports held by another process.
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, windows.WSAEACCES)
}
