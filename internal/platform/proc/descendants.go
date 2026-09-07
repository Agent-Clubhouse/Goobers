package proc

// collectDescendants returns every pid reachable from root through parents,
// breadth first, visiting each pid at most once.
//
// The visited set is load-bearing, not hygiene (#3922). A process table is not
// a tree: on Windows the parent pid an entry carries is the CREATOR's pid at
// creation time and is never cleared when that creator exits, so once the pid
// is recycled the recorded parentage can point anywhere — including back into
// the subtree it came from. Linux's /proc is a tree at any instant, but this
// walk reads it one pid at a time, so an entry recorded before a reparent and
// one recorded after it can describe a cycle that never existed.
//
// Either shape makes an unguarded breadth-first walk enumerate the same pids
// forever. That is not a slow answer, it is no answer: Tree.kill calls this
// while terminating, so the caller spins inside the walk querying the same
// processes until the whole test binary hits its timeout — the ten-minute
// package panic reported in #3922, whose goroutine dump caught the loop in
// startTime with the walk on the stack.
func collectDescendants(root int, parents map[int][]int) []int {
	visited := map[int]bool{root: true}
	var descendants []int
	queue := append([]int(nil), parents[root]...)
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if visited[pid] {
			continue
		}
		visited[pid] = true
		descendants = append(descendants, pid)
		queue = append(queue, parents[pid]...)
	}
	return descendants
}
