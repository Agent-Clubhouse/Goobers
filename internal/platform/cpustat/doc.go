// Package cpustat reads the CPU budget the daemon's container is actually
// scheduled against, and the CFS throttling that budget has already cost.
//
// It is the CPU sibling of internal/platform/memstat. memstat exists because a
// pod-level memory number could not distinguish a leaking daemon from a cgroup
// filling with page cache (#3949); cpustat exists because there was no number
// at all on the CPU side. A container's CPU quota is invisible to every ordinary
// way a process asks "how many CPUs do I have": runtime.NumCPU, nproc, and
// os.cpus() all report the node's logical CPU count. On the prod AKS instance
// that gap read as 4 CPUs against a 3 CPU limit, and 79.5% of CFS periods
// throttled (#3963).
//
// Two callers, two needs. internal/procenv needs Procs — one integer, the
// parallelism budget stated to every stage and harness subprocess so the whole
// child tree agrees with the quota. The daemon heartbeat needs Read — the
// operator-facing reading, including the throttling counters, which are the
// CPU-side term with the property that made memstat's clause worth carrying:
// they show pressure that a point-in-time gauge cannot.
//
// Like memstat, every failure path yields "no cgroup" rather than an error. A
// developer machine, a non-Linux host, an unrecognized layout and a restricted
// mount are all ordinary outcomes, and a diagnostic a caller can be forced to
// handle an error from is a diagnostic that gets dropped from the path that
// needs it most.
package cpustat
