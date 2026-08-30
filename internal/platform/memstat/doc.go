// Package memstat reads the running process's memory footprint — the Go
// runtime's own accounting plus, where one is readable, the container memory
// cgroup it is charged against.
//
// The two halves answer different questions and a diagnosis needs both. The
// runtime half ("is the daemon growing?") sees only Go heap and runtime-managed
// spans. The cgroup half ("is the pod about to be killed?") sees every byte
// charged to the limit the OOM killer enforces: the runtime's anonymous memory,
// every child process the daemon spawns, and the page cache produced by all of
// their file I/O.
//
// Reporting only the first is how #3949 happened: goobers-api was OOMKilled
// while its own anonymous memory sat flat at tens of MiB, because the 10Gi
// cgroup had filled with page cache from an unbounded on-pod Go build cache.
// Only the cgroup's anon/file split distinguishes that from a real leak, and no
// runtime metric can see it.
package memstat
