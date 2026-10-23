package runtimewiring

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/goobers/goobers/internal/configtree"
	"github.com/goobers/goobers/internal/gooberassets"
)

// ConfigDirectoryDigest fingerprints the config directory so the reloader can
// tell a real change from a no-op. It tracks YAML definitions, their referenced
// goober instructions and skill bodies, and every file in a goober assets
// directory; unrelated config-tree churn remains excluded.
func ConfigDirectoryDigest(root string) (string, error) {
	return configDirectoryDigestScoped(root, "")
}

// ConfigDirectoryDigestForGaggle fingerprints the config surface visible to
// one gaggle. Sibling gaggle trees are independent hot-reload units and must
// not invalidate deterministic stages already running under this generation.
func ConfigDirectoryDigestForGaggle(root, gaggle string) (string, error) {
	if strings.TrimSpace(gaggle) == "" {
		return ConfigDirectoryDigest(root)
	}
	return configDirectoryDigestScoped(root, gaggle)
}

func configDirectoryDigestScoped(root, gaggle string) (string, error) {
	return ConfigDirectoryDigestObserved(root, gaggle, nil)
}

// ConfigDirectoryDigestObserved hashes config and captured persona assets while reporting each observed file.
func ConfigDirectoryDigestObserved(root, gaggle string, observe func(string)) (string, error) {
	hash := sha256.New()
	contentPaths := make(map[string]struct{})
	includePath := func(path string) (bool, error) {
		if gaggle == "" {
			return true, nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return false, err
		}
		parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
		return len(parts) < 2 || parts[0] != "gaggles" || parts[1] == gaggle, nil
	}
	writeEntry := func(path string, mode fs.FileMode, content []byte) error {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(relative)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(relative))
		binary.BigEndian.PutUint64(size[:], uint64(mode))
		_, _ = hash.Write(size[:])
		binary.BigEndian.PutUint64(size[:], uint64(len(content)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write(content)
		return nil
	}
	err := configtree.WalkDefinitionTrees(root, func(tree string) error {
		return filepath.WalkDir(tree, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			included, err := includePath(path)
			if err != nil {
				return err
			}
			if !included {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			name := entry.Name()
			// Handle asset loading/hashing first
			if gooberassets.IsSourceDir(path) {
				bundle, err := gooberassets.Load(path)
				if err != nil {
					return err
				}
				if bundle == nil {
					return nil
				}
				if err := writeEntry(path, 0, []byte(bundle.Fingerprint())); err != nil {
					return err
				}
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				// Skip hidden dirs and gaggle skills dirs
				if configtree.ShouldSkipConfigDirExcludingAssets(root, path) {
					return filepath.SkipDir
				}
				return nil
			}
			// Outside asset bundles, only YAML definitions contribute.
			if strings.HasPrefix(name, ".") {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".yaml" && ext != ".yml" {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				// A config file that vanished between the walk and the read (an
				// editor's atomic rename, a git checkout) is a transient state, not
				// a rejectable config. Skip it and let the next poll — with the
				// read-validate-reread stability check — converge on settled bytes.
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if err := writeEntry(path, 0, content); err != nil {
				return err
			}
			references, err := gooberContentReferences(root, path, content)
			if err != nil {
				return err
			}
			for _, contentPath := range references {
				contentPaths[contentPath] = struct{}{}
			}
			return nil
		})
	})
	if err != nil {
		return "", fmt.Errorf("digest config directory: %w", err)
	}
	paths := make([]string, 0, len(contentPaths))
	for path := range contentPaths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if observe != nil {
			observe(path)
		}
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("digest config directory: stat resolved goober content: %w", err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		content, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("digest config directory: read resolved goober content: %w", err)
		}
		if err := writeEntry(path, 0, content); err != nil {
			return "", fmt.Errorf("digest config directory: %w", err)
		}
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

type configDigestDocument struct {
	Kind string `json:"kind"`
	Spec struct {
		Instructions string   `json:"instructions"`
		Gaggle       string   `json:"gaggle"`
		Skills       []string `json:"skills"`
	} `json:"spec"`
}

func gooberContentReferences(configDir, definitionPath string, content []byte) ([]string, error) {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(content), 4096)
	var paths []string
	for {
		var document configDigestDocument
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return paths, nil
		}
		if err != nil {
			// The YAML bytes already move the digest; config validation owns the
			// diagnostic for malformed documents.
			return paths, nil
		}
		if document.Kind != "Goober" {
			continue
		}
		if document.Spec.Instructions != "" {
			paths = append(paths, filepath.Join(filepath.Dir(definitionPath), document.Spec.Instructions))
		}
		for _, skill := range document.Spec.Skills {
			_, skillPaths, ok, err := skillPackagePaths(configDir, document.Spec.Gaggle, skill)
			if err != nil {
				return nil, fmt.Errorf("list referenced skill %q package: %w", skill, err)
			}
			if ok {
				paths = append(paths, skillPaths...)
			}
		}
	}
}
