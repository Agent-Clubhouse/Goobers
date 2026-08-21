//go:build linux

package proc

// zombie reports whether pid names a process that has already exited and is
// only waiting for its parent to collect its exit status.
//
// A zombie answers a signal-0 probe as if it were alive — the pid is still
// allocated — so without this check Alive reports a dead process alive FOREVER
// whenever nobody ever wait()s for it. That is not the benign, self-correcting
// false-alive doc.go accepts: a permanent one converts "defer the reap" into
// "never reap", which is how a dead stage descendant pins its worktree for the
// life of the pod (#3399).
//
// It stays on the fail-toward-alive side of the asymmetry: only a /proc entry
// this process could actually read, reporting state 'Z', counts as dead. An
// unreadable or unparseable entry is "unknown", which reports not-zombie and
// therefore leaves Alive's answer alone.
func zombie(pid int) bool {
	stat, ok := readProcStat(pid)
	if !ok {
		return false
	}
	return stat.state == 'Z'
}
