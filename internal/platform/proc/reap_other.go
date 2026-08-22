//go:build !linux

package proc

import "context"

// startOrphanReaper is a no-op off linux. Reparenting orphans onto pid 1 with
// nobody to reap them is a linux-container problem: darwin and windows daemons
// are supervised processes (launchd, the SCM) that are never their namespace's
// init, and windows has no wait()/zombie model at all.
func startOrphanReaper(context.Context) bool {
	return false
}

// trackChild is likewise inert: with no reaper installed there is nothing that
// could steal an exec.Cmd's exit status, so nothing needs recording.
func trackChild(int) {}
