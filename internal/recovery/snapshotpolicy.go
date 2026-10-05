package recovery

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/mutationsidecar"
)

// SnapshotPolicy filters child workspace content before it is added to the
// private snapshot index. Excluded paths are omissions, never instructions to
// delete the same paths from a receiving parent. Ordinary recovery remains
// unfiltered. Callers add their known credential and injected runtime paths;
// this is path policy, not secret detection in arbitrary source files.
type SnapshotPolicy struct {
	ExcludedPaths []string `json:"excludedPaths,omitempty"`
}

// Validate bounds explicit paths and refuses patterns, traversal and ambiguous
// platform-specific spellings. Paths name a file or a complete directory tree.
func (p SnapshotPolicy) Validate() error {
	if len(p.ExcludedPaths) > 128 {
		return fmt.Errorf("snapshot policy exceeds 128 excluded paths")
	}
	seen := map[string]bool{}
	for _, name := range p.ExcludedPaths {
		if name == "" || len(name) > 512 || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\x00\r\n:*?[]") || name == "." || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("invalid snapshot exclusion path")
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("duplicate snapshot exclusion path")
		}
		seen[key] = true
	}
	return nil
}

func (p *SnapshotPolicy) excludes(name string) bool {
	if p == nil {
		return false
	}
	name = strings.ToLower(name)
	for _, prefix := range []string{".git", ".goobers", gooberassets.WorkspaceDir, mutationsidecar.FileName} {
		if name == prefix || strings.HasPrefix(name, prefix+"/") {
			return true
		}
	}
	if strings.HasPrefix(name, ".goobers-launcher-session-") {
		return true
	}
	for _, prefix := range p.ExcludedPaths {
		prefix = strings.ToLower(prefix)
		if name == prefix || strings.HasPrefix(name, prefix+"/") {
			return true
		}
	}
	return false
}

func filterSnapshotPaths(data []byte, policy *SnapshotPolicy) ([]byte, error) {
	if policy == nil {
		return data, nil
	}
	var result []byte
	for len(data) > 0 {
		name, rest, complete := bytes.Cut(data, []byte{0})
		if !complete {
			return nil, fmt.Errorf("invalid snapshot path list")
		}
		data = rest
		if !policy.excludes(string(name)) {
			result = append(result, name...)
			result = append(result, 0)
		}
	}
	return result, nil
}

func filterSnapshotIndex(data []byte, policy *SnapshotPolicy) ([]byte, error) {
	if policy == nil {
		return data, nil
	}
	var result []byte
	for len(data) > 0 {
		entry, rest, complete := bytes.Cut(data, []byte{0})
		if !complete {
			return nil, fmt.Errorf("invalid snapshot index entries")
		}
		data = rest
		_, name, ok := bytes.Cut(entry, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("invalid snapshot index entry")
		}
		if !policy.excludes(string(name)) {
			result = append(result, entry...)
			result = append(result, 0)
		}
	}
	return result, nil
}
