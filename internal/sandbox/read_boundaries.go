package sandbox

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
)

// ReadBoundary hides a host root except explicitly named private subdirectories.
// Unlike enumerated denials, newly created sibling sessions are hidden too.
type ReadBoundary struct {
	Root  string
	Allow []string
}

// WithReadBoundaries confines reads in shared runtime roots on every launch.
func WithReadBoundaries(base Sandbox, boundaries []ReadBoundary) Sandbox {
	return readBoundarySandbox{Sandbox: base, boundaries: cloneBoundaries(boundaries)}
}

type readBoundarySandbox struct {
	Sandbox
	boundaries []ReadBoundary
}

func cloneBoundaries(boundaries []ReadBoundary) []ReadBoundary {
	out := slices.Clone(boundaries)
	for i := range out {
		out[i].Allow = slices.Clone(out[i].Allow)
	}
	return out
}

func (s readBoundarySandbox) Wrap(command *exec.Cmd, policy Policy) error {
	policy.ReadBoundaries = append(cloneBoundaries(policy.ReadBoundaries), cloneBoundaries(s.boundaries)...)
	return s.Sandbox.Wrap(command, policy)
}

func validateReadBoundaries(boundaries []ReadBoundary, writable []string) ([]ReadBoundary, error) {
	if len(boundaries) > 16 {
		return nil, fmt.Errorf("sandbox: too many read boundaries")
	}
	result := make([]ReadBoundary, 0, len(boundaries))
	for _, boundary := range boundaries {
		root, err := resolveDirectory(boundary.Root)
		if err != nil || root == filepath.VolumeName(root)+string(filepath.Separator) || len(boundary.Allow) > 16 {
			return nil, fmt.Errorf("sandbox: invalid read boundary")
		}
		resolved := ReadBoundary{Root: root}
		for _, exception := range boundary.Allow {
			path, err := resolveDirectory(exception)
			if err != nil || path == root || !pathContains(root, path) {
				return nil, fmt.Errorf("sandbox: exception is not strictly inside read boundary")
			}
			resolved.Allow = append(resolved.Allow, path)
		}
		if err := checkBoundaryWrites(resolved, writable); err != nil {
			return nil, err
		}
		for _, existing := range result {
			if pathContains(existing.Root, root) || pathContains(root, existing.Root) {
				return nil, fmt.Errorf("sandbox: read boundaries must not overlap")
			}
		}
		result = append(result, resolved)
	}
	return result, nil
}

func checkBoundaryWrites(boundary ReadBoundary, writable []string) error {
	for _, root := range writable {
		if pathContains(root, boundary.Root) {
			return fmt.Errorf("sandbox: writable ancestor could move a read boundary")
		}
		if pathContains(boundary.Root, root) && !slices.ContainsFunc(boundary.Allow, func(allowed string) bool {
			return pathContains(allowed, root)
		}) {
			return fmt.Errorf("sandbox: writable path lies outside allowed read exception")
		}
	}
	return nil
}
