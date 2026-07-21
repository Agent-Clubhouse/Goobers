// Package proc provides cross-platform control of a spawned command's whole
// descendant process tree — the mechanism the runner relies on to guarantee an
// agent harness (Copilot CLI etc.) and every subprocess it spawns die together,
// never orphaning a tree that keeps holding a stage's stdout/stderr pipes open.
//
// It exists so call sites never branch on runtime.GOOS or reach for
// platform-specific syscalls directly. Following the convention of
// internal/platform/lock, the surface here is small and the behavior lives in
// build-tagged files:
//
//   - Configure arranges tree ownership on an *exec.Cmd before it starts.
//   - Start starts a Configure-d command and returns a Tree handle.
//   - Tree.Kill hard-terminates the entire tree.
//   - Tree.RequestDump asks the tree to emit diagnostics and exit (unix only).
//   - Alive reports whether a pid names a live process.
//
// On unix a tree is a process group: Configure puts the child in a new session
// (Setsid), so its process-group id equals its pid, and Kill signals the whole
// negative-pid group. On windows (the explicit #623 follow-up) the same seam is
// implemented with Job Objects — assign the child to a job with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so a daemon crash reaps the tree, and
// TerminateJobObject for Kill — with no change to any call site.
//
// Liveness has an asymmetric failure cost the implementations must honor: a
// false "dead" is destructive (the worktree reaper would delete a live run's
// worktree), while a false "alive" merely defers a reap. Alive therefore fails
// toward alive on an ambiguous probe (see the unix implementation's EPERM
// handling).
package proc
