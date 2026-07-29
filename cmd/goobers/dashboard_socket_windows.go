//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// dashboardPortUnavailable reports whether a listen failure means "this
// particular port cannot be used", as opposed to a real error that should stop
// the search. Under --port=auto the caller skips to the next port for these.
//
// WSAEACCES matters as much as WSAEADDRINUSE on Windows: Hyper-V and WinNAT
// reserve blocks inside the dynamic range (visible via
// `netsh interface ipv4 show excludedportrange tcp`), and binding one of those
// fails with "An attempt was made to access a socket in a way forbidden by its
// access permissions" rather than an in-use error. Treating that as fatal made
// --port=auto abort the moment it walked into a reserved block, which is a
// normal thing to hit on a developer machine.
func dashboardPortUnavailable(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, windows.WSAEACCES)
}
