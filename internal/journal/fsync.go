package journal

import "os"

// envDisableFsync, when set to "1", makes every durability fsync in this
// package a no-op for the current process. It is the journal-side companion to
// the git fsync-disable seam cmd/goobers tests already use (disableGitFsyncForTests).
const envDisableFsync = "GOOBERS_DISABLE_FSYNC"

// fsyncDisabled reports whether journal fsyncs are skipped for this process.
//
// It is read per call (never cached at package init) on purpose: a test process
// enables it from TestMain — which runs AFTER package init — so an init-time
// read would miss it. The cost is a single os.Getenv (~tens of ns) against a
// real fsync (~ms) it may replace, so per-call reading is free relative to the
// work.
//
// Why this exists — the journal edition of the #811 fsync hang: fsync is the one
// syscall that blocks in uninterruptible I/O under disk saturation. A `make ci`
// runs several cold `go test -race ./...` at once, and the cmd/goobers suite
// spins up real in-process runs that fsync every journal event, checkpoint, and
// artifact. Under that combined write pressure a single journal fsync can wedge
// for the whole 10-minute stage, so `goobers run`'s waitForRunTerminal polls a
// run that never finishes and the local-ci stage times out having opened 0 PRs.
// #811 disabled git subprocess fsync for the same reason; the journal's own
// os.File.Sync() was never covered. Test instances are ephemeral t.TempDir
// scratch with zero durability requirements — skipping fsync only changes what a
// crash mid-test would leave behind, and a test deletes its scratch anyway.
// Production leaves the env unset, so crash durability is unchanged.
func fsyncDisabled() bool {
	return os.Getenv(envDisableFsync) == "1"
}

// syncFile fsyncs f unless fsync is disabled for this process.
func syncFile(f *os.File) error {
	if fsyncDisabled() {
		return nil
	}
	return f.Sync()
}
