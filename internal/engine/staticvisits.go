package engine

import (
	"maps"
	"slices"
)

const staticParallelVisitsVersion = "engine-static-parallel-visits-v1"

// advanceStaticJoin scopes the frozen fan-in view to its immediate join.
// Returning to that stage later must not resurrect an old visit's completeness.
func (r *runJournal) advanceStaticJoin(state string) {
	s := r.staticWalk
	if s == nil || !s.currentVisits || s.join == "" || s.join == state {
		return
	}
	s.join, s.parallel = "", ""
	s.completeness, s.branches = nil, nil
}

// mergeVisit replaces only products actually visited by each branch. Copying a
// whole seed would overwrite a sibling's fresh result with its previous visit;
// retaining existing root entries would leave the previous visit forever.
func (p *staticParallelWalk) mergeVisit() {
	for _, out := range p.outcomes {
		frame := out.frame
		for _, stage := range slices.Sorted(maps.Keys(frame.visited)) {
			p.seed.upstream[stage] = frame.upstream[stage]
			if completed, ok := frame.completed[stage]; ok {
				p.seed.completed[stage] = completed
			} else {
				delete(p.seed.completed, stage)
			}
		}
		if len(frame.visited) > 0 {
			p.seed.lastStage, p.seed.lastResult = frame.lastStage, frame.lastResult
		}
	}
	// Pointer suffixes and evaluated gates are merged without replaying the
	// inherited scalar seeds, which could reintroduce a forgotten output.
	p.mergeMetadata()
}
