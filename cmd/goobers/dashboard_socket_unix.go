//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// dashboardPortUnavailable reports whether a listen failure means "this
// particular port cannot be used", as opposed to a real error that should stop
// the search. Under --port=auto the caller skips to the next port for these.
//
// Unix has no equivalent of Windows' reserved dynamic-range blocks, so an
// in-use port is the only skippable case here. EACCES on Unix means the port is
// privileged (<1024), which incrementing cannot fix and should stay fatal.
func dashboardPortUnavailable(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}
