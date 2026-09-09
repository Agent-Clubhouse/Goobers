package instance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// RootDiscovery is explicitly partial when a bound or unreadable directory
// prevents complete inspection. Finding a config file is not proof of liveness.
type RootDiscovery struct {
	Paths   []string `json:"paths"`
	Partial bool     `json:"partial"`
	Visited int      `json:"visited"`
}

// DiscoverRoots performs bounded, read-only discovery without following child
// symlinks. Search bases may themselves be symlinks and are canonicalized once.
// Instance interiors are not traversed: runs and checkouts are not other roots.
func DiscoverRoots(ctx context.Context, bases []string, maxDirectories, maxDepth int) (RootDiscovery, error) {
	result := RootDiscovery{Paths: []string{}}
	if maxDirectories < 1 || maxDepth < 0 {
		return result, fmt.Errorf("invalid root discovery bounds")
	}
	seen := make(map[string]int)
	found := make(map[string]bool)
	remainingEntries := maxDirectories
	var visit func(string, int) error
	visit = func(path string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if previousDepth, ok := seen[path]; ok && previousDepth <= depth {
			return nil
		}
		if result.Visited >= maxDirectories {
			result.Partial = true
			return nil
		}
		seen[path] = depth
		result.Visited++
		if info, err := os.Lstat(NewLayout(path).ConfigFile()); err == nil && info.Mode().IsRegular() {
			if !found[path] {
				result.Paths = append(result.Paths, path)
				found[path] = true
			}
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			result.Partial = true
			return nil
		}
		defer func() { _ = f.Close() }()
		return readDiscoveryChildren(ctx, f, path, depth, maxDepth, &remainingEntries, &result, visit)
	}
	for _, base := range bases {
		absolute, err := filepath.Abs(base)
		if err != nil {
			return result, err
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			result.Partial = true
			continue
		}
		if err := visit(resolved, 0); err != nil {
			return result, err
		}
	}
	sort.Strings(result.Paths)
	return result, nil
}

func readDiscoveryChildren(ctx context.Context, f *os.File, path string, depth, maxDepth int, remainingEntries *int, result *RootDiscovery, visit func(string, int) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if *remainingEntries == 0 {
			result.Partial = true
			return nil
		}
		entries, readErr := f.ReadDir(128)
		for _, entry := range entries {
			if *remainingEntries == 0 {
				result.Partial = true
				return nil
			}
			*remainingEntries--
			if !entry.IsDir() || discoveryIgnoredDirectory(entry.Name()) {
				continue
			}
			if depth >= maxDepth {
				result.Partial = true
				continue
			}
			if err := visit(filepath.Join(path, entry.Name()), depth+1); err != nil {
				return err
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				result.Partial = true
			}
			return nil
		}
	}
}

func discoveryIgnoredDirectory(name string) bool {
	switch name {
	case ".git", "node_modules", ".cache", "Library", "vendor":
		return true
	default:
		return false
	}
}
