package proc

import "time"

// StartTime returns the wall-clock time the process named by pid started,
// and whether it could be determined. ok is false when the process does not
// exist or its start time could not be read.
//
// Combined with Alive, this resolves the PID-reuse ambiguity Alive's own doc
// warns about (#2052): a marker's recorded PID can be "alive" yet belong to
// an entirely different process if the original process exited and the OS
// later recycled its PID onto a new, long-lived process — Alive alone cannot
// tell the two apart. A caller that also recorded the ORIGINAL process's
// start time (at marker-creation time) can detect reuse by comparing it
// against StartTime's live answer for the same PID: a real process's start
// time is immutable for its whole lifetime, so a mismatch unambiguously means
// the PID now names a different process.
func StartTime(pid int) (time.Time, bool) {
	return startTime(pid)
}
