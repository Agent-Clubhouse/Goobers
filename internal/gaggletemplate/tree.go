// Package gaggletemplate manages opt-in template ancestry independently of
// executable gaggle definitions.
package gaggletemplate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// MetadataDir is excluded from executable definitions and pristine file trees.
const MetadataDir = ".template"
const maxTreeBytes = 16 << 20
const maxFiles = 2048

// File preserves both content and permissions in a merge baseline.
type File struct {
	Data []byte `json:"data"`
	Mode uint32 `json:"mode"`
}

// Tree indexes package files by portable, slash-separated relative paths.
type Tree map[string]File

func safeRelative(name string) bool {
	return name != "." && filepath.IsLocal(name) && !strings.ContainsAny(name, "\\:") &&
		!strings.HasPrefix(name, "/") && filepath.ToSlash(filepath.Clean(name)) == name
}

// ReadTree captures regular files without following links. Template packages
// are intentionally bounded and self-contained; hidden management is excluded.
func ReadTree(root string) (Tree, error) {
	tree := Tree{}
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == MetadataDir || rel == ".git" {
			if !entry.IsDir() {
				return fmt.Errorf("reserved directory %s is not a directory", path)
			}
			return filepath.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("template path %s is a symlink", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !safeRelative(rel) {
			return fmt.Errorf("unsafe template file %s", path)
		}
		total += info.Size()
		if total > maxTreeBytes || len(tree) >= maxFiles {
			return fmt.Errorf("template exceeds %d files or %d bytes", maxFiles, maxTreeBytes)
		}
		data, err := readBounded(path, maxTreeBytes)
		if err != nil {
			return err
		}
		tree[rel] = File{Data: data, Mode: uint32(info.Mode().Perm())}
		return nil
	})
	return tree, err
}

func readBounded(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds size limit", path)
	}
	return data, nil
}

// Validate rejects unsafe paths, portable path collisions and oversized trees.
func (t Tree) Validate() error {
	total := 0
	folded := map[string]bool{}
	for name, file := range t {
		if !safeRelative(name) || strings.Split(name, "/")[0] == MetadataDir ||
			strings.Split(name, "/")[0] == ".git" || file.Mode&^0777 != 0 {
			return fmt.Errorf("unsafe template entry %q", name)
		}
		key := strings.ToLower(name)
		if folded[key] {
			return fmt.Errorf("case-insensitive template path collision %q", name)
		}
		folded[key] = true
		total += len(file.Data)
	}
	if total > maxTreeBytes || len(t) > maxFiles {
		return errors.New("template tree exceeds size limit")
	}
	return nil
}

// Digest fingerprints paths, content and permissions deterministically.
func (t Tree) Digest() string {
	data, _ := json.Marshal(t) // Tree contains only JSON-supported values.
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Write materializes validated entries under a caller-owned staging directory.
func (t Tree) Write(root string) error {
	if err := t.Validate(); err != nil {
		return err
	}
	for _, name := range sortedKeys(t) {
		file := t[name]
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, file.Data, os.FileMode(file.Mode)); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys(t Tree) []string {
	keys := make([]string, 0, len(t))
	for key := range t {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func equalFile(a File, aOK bool, b File, bOK bool) bool {
	return aOK == bOK && (!aOK || reflect.DeepEqual(a, b))
}

// Publish swaps a validated candidate for one gaggle. A retained backup blocks
// further publication after interruption instead of guessing which copy won.
func Publish(target, candidate string, expected Tree) error {
	backup := filepath.Join(filepath.Dir(target), ".template-backup-"+filepath.Base(target))
	if _, err := os.Lstat(backup); err == nil {
		return fmt.Errorf("template recovery required: inspect existing backup %s", backup)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("template recovery required: inspect %s: %w", backup, err)
	}
	current, err := ReadTree(target)
	if errors.Is(err, fs.ErrNotExist) && expected == nil {
		return os.Rename(candidate, target)
	}
	if err != nil {
		return err
	}
	if current.Digest() != expected.Digest() {
		return errors.New("gaggle changed during operation; retry without overwriting the newer edits")
	}
	if err := os.Rename(target, backup); err != nil {
		return err
	}
	if err := os.Rename(candidate, target); err != nil {
		return errors.Join(err, os.Rename(backup, target))
	}
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("gaggle published, but recovery backup cleanup failed: %w", err)
	}
	return nil
}
