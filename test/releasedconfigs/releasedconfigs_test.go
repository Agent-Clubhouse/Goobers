// Package releasedconfigs holds configs that a released Goobers accepted and
// proves the current tree still accepts them.
//
// Two regressions motivated it. v0.6.0-alpha.1 made an empty requireLabels on
// a DSL 2.0 "implementation" workflow a hard error (LCL001), and
// v0.6.0-alpha.3 moved pr-select from github:pr:write to provider:pr:write.
// Both broke existing instances on upgrade, and CI passed both times: every
// test validated the configs this repo ships, and both changes updated those
// configs in the same commit. Nothing validated a config written before the
// change.
//
// testdata/released/<tag> is the shipped config-examples tree of that tag.
// testdata/user-authored holds shapes users write that the shipped examples
// do not exercise. Both are frozen: corpus.lock.json pins their content, so a
// test failure here cannot be fixed by editing the corpus. Fix the change
// instead: keep accepting the old form, or gate the new rule behind a new DSL
// version (providerstage sinceDSL/untilDSL, or a dslVersion check in
// api/validate).
package releasedconfigs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/validate"
)

const lockFile = "corpus.lock.json"

func corpora(t *testing.T) map[string]string {
	t.Helper()
	dirs := map[string]string{"user-authored": filepath.Join("testdata", "user-authored")}
	released, err := os.ReadDir(filepath.Join("testdata", "released"))
	if err != nil {
		t.Fatalf("read released corpora: %v", err)
	}
	for _, entry := range released {
		if entry.IsDir() {
			dirs["released/"+entry.Name()] = filepath.Join("testdata", "released", entry.Name())
		}
	}
	if len(dirs) < 2 {
		t.Fatal("no released corpus found under testdata/released")
	}
	return dirs
}

func TestReleasedConfigsStillValidate(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatalf("validate.New: %v", err)
	}
	for name, dir := range corpora(t) {
		t.Run(name, func(t *testing.T) {
			report, err := v.ValidateDir(dir)
			if err != nil {
				t.Fatalf("ValidateDir(%s): %v", dir, err)
			}
			var errs []string
			for _, issue := range report.Issues {
				if issue.Severity == validate.Error {
					errs = append(errs, issue.String())
				}
			}
			if len(errs) > 0 {
				t.Fatalf("a config that a released Goobers accepted no longer validates. Existing instances would break on upgrade. "+
					"Keep accepting the old form or gate the new rule behind a new DSL version; do not edit this corpus.\n%s",
					strings.Join(errs, "\n"))
			}
		})
	}
}

func TestCorpusIsFrozen(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", lockFile))
	if err != nil {
		t.Fatalf("read %s: %v", lockFile, err)
	}
	var lock map[string]string
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatalf("decode %s: %v", lockFile, err)
	}
	dirs := corpora(t)
	for name, dir := range dirs {
		got, err := corpusDigest(dir)
		if err != nil {
			t.Fatalf("digest %s: %v", dir, err)
		}
		want, ok := lock[name]
		switch {
		case !ok:
			t.Errorf("corpus %q is not in testdata/%s; add \"%s\": %q when adding a corpus", name, lockFile, name, got)
		case got != want:
			t.Errorf("corpus %q changed (sha256 %s, locked %s). The corpus records what released versions accepted and must not be edited to make a test pass", name, got, want)
		}
	}
	for name := range lock {
		if _, ok := dirs[name]; !ok {
			t.Errorf("testdata/%s pins %q, which has no corpus directory", lockFile, name)
		}
	}
}

// corpusDigest hashes every file's slash path and content, with CRLF
// normalized so the digest does not depend on checkout line endings.
func corpusDigest(dir string) (string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", err
	}
	slices.Sort(files)
	h := sha256.New()
	for _, rel := range files {
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		normalized := strings.ReplaceAll(string(content), "\r\n", "\n")
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00", rel, len(normalized), normalized)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
