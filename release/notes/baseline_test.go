package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
)

type directoryGit string

func (dir directoryGit) output(args ...string) (string, error) {
	out, err := testgit.Command(append([]string{"-C", string(dir)}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func TestConfiguredBaselineOnSeparateReleaseBranch(t *testing.T) {
	git := directoryGit(t.TempDir())
	gitRun := func(args ...string) string {
		t.Helper()
		out, err := git.output(args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	gitRun("init", "-b", "main")
	gitRun("config", "user.name", "Release test")
	gitRun("config", "user.email", "release-test@example.invalid")
	commit := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(string(git), name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		gitRun("add", name)
		gitRun("commit", "-m", "fix: "+name)
	}
	commit("initial")
	gitRun("tag", "v0.4.0")
	gitRun("switch", "-c", "release/v0.5")
	commit("release-only")
	gitRun("tag", "v0.5.0")
	gitRun("switch", "main")
	commit("new-fix")
	for _, tag := range []string{"v0.6.0-alpha.2", "v0.6.0-alpha.3", "v0.6.0-alpha.4", "v0.6.0"} {
		gitRun("tag", tag)
	}
	config, err := os.ReadFile(filepath.Join("..", "..", ".github", "release-baselines.json"))
	if err != nil {
		t.Fatal(err)
	}
	readFile := func(path string) ([]byte, error) {
		if filepath.Base(path) == "release-baselines.json" {
			return config, nil
		}
		return []byte("Curated highlights."), nil
	}
	for _, tag := range []string{"v0.6.0-alpha.2", "v0.6.0-alpha.3", "v0.6.0-alpha.4", "v0.6.0"} {
		t.Run(tag, func(t *testing.T) {
			fallback, err := previousTag(tag, git)
			if err != nil || fallback != "v0.4.0" {
				t.Fatalf("ancestor fallback = %q, %v", fallback, err)
			}
			notes, err := generate(tag, git, readFile)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(notes, "Changes since `v0.5.0`.") || !strings.Contains(notes, "new-fix") || strings.Contains(notes, "initial (`") || strings.Contains(notes, "release-only (`") {
				t.Fatalf("incorrect release range:\n%s", notes)
			}
			// Snapshot downloads must resolve the identical tag, without curated notes.
			var stdout, stderr bytes.Buffer
			configOnly := func(path string) ([]byte, error) {
				if filepath.Base(path) != "release-baselines.json" {
					t.Fatalf("unexpected note read: %s", path)
				}
				return config, nil
			}
			if err := run([]string{"-tag", tag, "-print-baseline"}, &stdout, &stderr, git, configOnly); err != nil {
				t.Fatal(err)
			}
			if stdout.String() != "v0.5.0\n" {
				t.Fatalf("snapshot baseline = %q", stdout.String())
			}
		})
	}
}

func TestConfiguredBaselineValidation(t *testing.T) {
	for _, tc := range []struct {
		name, data, want string
		git              fakeGit
		readErr          error
	}{
		{name: "malformed", data: `{`, want: "decode release baselines"},
		{name: "read failure", readErr: os.ErrPermission, want: "read release baselines"},
		{name: "empty baseline", data: `{"v0.6.0":""}`, want: "earlier stable release"},
		{name: "prerelease", data: `{"v0.6.0":"v0.5.0-rc.5"}`, want: "earlier stable release"},
		{name: "same version", data: `{"v0.6.0":"v0.6.0"}`, want: "earlier stable release"},
		{name: "future version", data: `{"v0.6.0":"v0.10.0"}`, want: "earlier stable release"},
		{name: "missing tag", data: `{"v0.6.0":"v0.5.0"}`, want: "resolve configured baseline"},
		{name: "unrelated history", data: `{"v0.6.0":"v0.5.0"}`, want: "must share release history", git: fakeGit{
			command("rev-parse", "--verify", "refs/tags/v0.5.0^{commit}"):         {output: "stable"},
			command("merge-base", "refs/tags/v0.5.0", "refs/tags/v0.6.0-alpha.4"): {err: errors.New("no merge base")},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := releaseBaseline("v0.6.0-alpha.4", tc.git, func(string) ([]byte, error) { return []byte(tc.data), tc.readErr })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestUnconfiguredReleaseUsesAncestorBaseline(t *testing.T) {
	git := fakeGit{
		command("rev-list", "--parents", "-n", "1", "v0.7.0"):                            {output: "release parent"},
		command("tag", "--merged", "v0.7.0^", "--sort=-version:refname", "--list", "v*"): {output: "v0.7.0-alpha.1\nv0.6.0"},
	}
	got, err := releaseBaseline("v0.7.0", git, func(string) ([]byte, error) { return []byte(`{"v0.6.0":"v0.5.0"}`), nil })
	if err != nil || got != "v0.6.0" {
		t.Fatalf("baseline = %q, %v", got, err)
	}
}

func TestPrintBaselineRejectsNoteOutputOptions(t *testing.T) {
	for _, extra := range []string{"-output", "-feature-notes"} {
		var stdout, stderr bytes.Buffer
		err := run([]string{"-tag", "v0.6.0", "-print-baseline", extra, "notes.md"}, &stdout, &stderr, fakeGit{}, missingFile)
		if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Fatalf("error = %v", err)
		}
	}
}
