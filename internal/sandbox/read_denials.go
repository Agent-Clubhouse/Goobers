package sandbox

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const (
	maxReadDeniedPaths   = 256
	maxReadDeniedEntries = 4096
	maxReadDeniedDepth   = 32
)

var errReadDeniedPath = errors.New("sandbox: guarded credential path cannot be safely confined")

type readDeniedPath struct {
	path      string
	directory bool
}

// WithReadDenials adds private paths to every command policy, including recovery
// turns. It does not enable a sandbox or relax the caller's admission checks.
func WithReadDenials(base Sandbox, paths []string) Sandbox {
	if len(paths) == 0 {
		return base
	}
	return readDeniedSandbox{Sandbox: base, paths: slices.Clone(paths)}
}

type readDeniedSandbox struct {
	Sandbox
	paths []string
}

func (s readDeniedSandbox) Wrap(command *exec.Cmd, policy Policy) error {
	policy.ReadDeniedPaths = append(slices.Clone(policy.ReadDeniedPaths), s.paths...)
	return s.Sandbox.Wrap(command, policy)
}

func validateReadDenials(paths []string, writable []string) ([]readDeniedPath, error) {
	if len(paths) > maxReadDeniedPaths {
		return nil, errReadDeniedPath
	}
	byPath := make(map[string]readDeniedPath, len(paths))
	remaining := maxReadDeniedEntries
	for _, path := range paths {
		denied, err := resolveReadDenial(path, writable)
		if err != nil {
			return nil, err
		}
		if denied.directory {
			if err := validateReadDeniedDirectory(denied.path, 0, &remaining); err != nil {
				return nil, err
			}
		}
		byPath[denied.path] = denied
	}
	keys := make([]string, 0, len(byPath))
	for path := range byPath {
		keys = append(keys, path)
	}
	slices.Sort(keys)
	var out []readDeniedPath
	for _, path := range keys {
		if slices.ContainsFunc(out, func(parent readDeniedPath) bool { return parent.directory && pathContains(parent.path, path) }) {
			continue
		}
		out = append(out, byPath[path])
	}
	return out, nil
}

func resolveReadDenial(path string, writable []string) (readDeniedPath, error) {
	if path == "" {
		return readDeniedPath{}, errReadDeniedPath
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return readDeniedPath{}, errReadDeniedPath
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return readDeniedPath{}, errReadDeniedPath
	}
	info, err := os.Stat(resolved)
	if err != nil || (!info.IsDir() && (!info.Mode().IsRegular() || !singleLink(info))) {
		return readDeniedPath{}, errReadDeniedPath
	}
	for _, root := range writable {
		// A writable ancestor could move the guarded path out from under a mask.
		if pathContains(root, resolved) || (info.IsDir() && pathContains(resolved, root)) {
			return readDeniedPath{}, errReadDeniedPath
		}
	}
	return readDeniedPath{resolved, info.IsDir()}, nil
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validateReadDeniedDirectory rejects aliases that could escape a directory
// mask. ReadDir batches bound memory and a shared budget bounds all configured
// directory walks. This inspects metadata only, never credential contents.
func validateReadDeniedDirectory(path string, depth int, remaining *int) error {
	if depth >= maxReadDeniedDepth {
		return errReadDeniedPath
	}
	directory, err := os.Open(path)
	if err != nil {
		return errReadDeniedPath
	}
	defer directory.Close()
	for {
		entries, err := directory.ReadDir(64)
		if err != nil && !errors.Is(err, io.EOF) {
			return errReadDeniedPath
		}
		for _, entry := range entries {
			*remaining--
			if *remaining < 0 {
				return errReadDeniedPath
			}
			info, err := entry.Info()
			if err != nil {
				return errReadDeniedPath
			}
			if info.IsDir() {
				if err := validateReadDeniedDirectory(filepath.Join(path, entry.Name()), depth+1, remaining); err != nil {
					return err
				}
			} else if !info.Mode().IsRegular() || !singleLink(info) {
				return errReadDeniedPath
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}
