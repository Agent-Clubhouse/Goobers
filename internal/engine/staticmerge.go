package engine

import (
	"maps"
	"slices"
)

// merge retains root state and adds each branch's products in declaration
// order. The branch frames contain frozen root prefixes; those prefixes must
// not overwrite a prior branch's newly completed stages or duplicate pointers.
// Branch-qualified scalars resolve separately against settled visit products,
// so inherited root entries cannot masquerade as a branch result.
func (p *staticParallelWalk) merge() {
	rootPointers := len(p.seed.pointers)
	for i, out := range p.outcomes {
		frame := out.frame
		for _, stage := range slices.Sorted(maps.Keys(frame.upstream)) {
			if _, exists := p.seed.upstream[stage]; !exists {
				p.seed.upstream[stage] = frame.upstream[stage]
			}
		}
		for _, stage := range slices.Sorted(maps.Keys(frame.completed)) {
			if _, exists := p.seed.completed[stage]; !exists {
				p.seed.completed[stage] = frame.completed[stage]
			}
		}
		for _, gate := range slices.Sorted(maps.Keys(frame.evaluatedGates)) {
			p.seed.evaluatedGates[gate] = frame.evaluatedGates[gate]
		}
		if len(frame.pointers) < rootPointers {
			continue // A queued, cancelled body never received the seed.
		}
		for _, pointer := range frame.pointers[rootPointers:] {
			pointer.Branch, pointer.BranchName = i+1, p.spec.Branches[i].Name
			p.seed.pointers = append(p.seed.pointers, pointer)
		}
	}
}
