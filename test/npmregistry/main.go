// Command npmregistry rejects npm lockfile tarball URLs that resolve to
// anything but the public registry.
//
// A lockfile written on a machine configured against a private mirror records
// that mirror in every `resolved` URL it touched. The mirror answers 403 to
// everyone outside its ACL, so `npm ci` then fails for every other checkout —
// including this project's own cloud runner, whose local-ci gate reads the
// failure as a defect in whatever change happened to be in flight. It has
// happened twice (#1607, and again via the 1ES feed URLs #3907 carried in),
// which is why it is a gate and not a convention.
package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const publicRegistryHost = "registry.npmjs.org"

const lockfileName = "package-lock.json"

type lockfile struct {
	Packages map[string]struct {
		Resolved string `json:"resolved"`
	} `json:"packages"`
}

func main() {
	if err := verify("."); err != nil {
		fmt.Fprintf(os.Stderr, "npmregistry: %v\n", err)
		os.Exit(1)
	}
}

func verify(root string) error {
	paths, err := lockfilePaths(root)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := verifyLockfile(root, path); err != nil {
			return err
		}
	}
	return nil
}

func lockfilePaths(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "node_modules", ".git":
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() != lockfileName {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	sort.Strings(paths)
	return paths, nil
}

func verifyLockfile(root, relativePath string) error {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relativePath)))
	if err != nil {
		return fmt.Errorf("read %s: %w", relativePath, err)
	}
	var parsed lockfile
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("parse %s: %w", relativePath, err)
	}
	names := make([]string, 0, len(parsed.Packages))
	for name := range parsed.Packages {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		resolved := parsed.Packages[name].Resolved
		// A workspace or link entry has no tarball, and npm records relative
		// paths for them; only remote fetches carry a host to check.
		if resolved == "" || !strings.Contains(resolved, "://") {
			continue
		}
		parsedURL, err := url.Parse(resolved)
		if err != nil {
			return fmt.Errorf("%s: package %q has unparseable resolved URL %q: %w", relativePath, name, resolved, err)
		}
		if parsedURL.Hostname() == publicRegistryHost {
			continue
		}
		return fmt.Errorf(
			"%s: package %q resolves to %q, but every tarball must come from %s — "+
				"re-run `npm install` with the public registry so the lockfile is fetchable outside that mirror",
			relativePath, name, parsedURL.Hostname(), publicRegistryHost)
	}
	return nil
}
