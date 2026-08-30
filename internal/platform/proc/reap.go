package proc

import "context"

// StartOrphanReaper installs the minimal container-init child-reaping contract
// and reports whether it did.
//
// It installs ONLY when this process is pid 1 of its pid namespace on linux —
// the one arrangement in which the kernel reparents unrelated orphans onto it
// and no other init will ever wait() for them. The shipped image puts the
// daemon in exactly that position (`ENTRYPOINT ["goobers"]`, exec form, no init
// wrapper), so every stage descendant that outlives its parent — a double-fork,
// or a descendant whose parent Kill reaches first — reparents to the daemon and,
// with nothing reaping, stays a zombie for the life of the pod (#3398).
//
// A developer's local `goobers up` is never pid 1, so it takes the false branch
// and keeps today's behavior exactly: no signal handler, no registry, no cost.
//
// The reaper runs until ctx is cancelled. Calling it more than once per process
// installs more than one loop, so it belongs at daemon startup only.
func StartOrphanReaper(ctx context.Context) bool {
	return startOrphanReaper(ctx)
}
