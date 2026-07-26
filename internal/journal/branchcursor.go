package journal

// reconstructBranchCursors rebuilds the per-branch resume positions from the
// event log. It returns ok=false when the run is not inside a live parallel —
// either none ever started, or the last one finished and the run is
// single-cursor again.
//
// This is the FO-3 counterpart to the runner's own backward "last X wins"
// resume scans, and it exists for the same reason those must become
// branch-scoped: a totally-ordered scan over an interleaved log attributes the
// wrong event to the wrong branch. Here every decision is keyed on
// (Branch, Seq), so interleaving cannot corrupt it.
func reconstructBranchCursors(events []Event) ([]BranchCursor, bool) {
	// Find the last parallel that started and has not finished. Events are in
	// seq order, and parallel lifecycle events are recorded on the root
	// branch, so a forward scan is exact.
	var openParallel string
	for _, e := range events {
		switch e.Type {
		case EventParallelStarted:
			openParallel = e.Parallel
		case EventParallelFinished:
			if e.Parallel == openParallel {
				openParallel = ""
			}
		}
	}
	if openParallel == "" {
		return nil, false
	}

	// Declaration order is normative (it assigns branch ids), so cursors are
	// ordered by branch id rather than by whatever order events arrived in.
	type cursor struct {
		BranchCursor
		seen bool
	}
	byID := map[int]*cursor{}
	var order []int

	for _, e := range events {
		if e.Parallel != openParallel && e.Branch == 0 {
			continue
		}
		switch e.Type {
		case EventBranchStarted:
			if e.Parallel != openParallel {
				continue
			}
			c, ok := byID[e.Branch]
			if !ok {
				c = &cursor{}
				byID[e.Branch] = c
				order = append(order, e.Branch)
			}
			c.Branch = e.Branch
			c.Name = e.BranchName
			c.Parallel = e.Parallel
			c.MachineState = e.Stage
			c.Status = ""
			c.seen = true
		case EventBranchFinished:
			if e.Parallel != openParallel {
				continue
			}
			if c, ok := byID[e.Branch]; ok {
				c.Status = e.BranchStatus
				// A settled branch has no resume position.
				c.MachineState = ""
			}
		case EventStageStarted, EventGateEvaluated:
			// Advance the owning branch's cursor. Keyed on Branch, so a
			// sibling's interleaved events never move this cursor.
			if c, ok := byID[e.Branch]; ok && c.Status == "" {
				if e.Stage != "" {
					c.MachineState = e.Stage
				} else if e.Gate != "" {
					c.MachineState = e.Gate
				}
			}
		}
	}

	if len(order) == 0 {
		return nil, false
	}
	sortInts(order)
	out := make([]BranchCursor, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id].BranchCursor)
	}
	return out, true
}

func sortInts(v []int) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func equalBranchCursors(a, b []BranchCursor) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
