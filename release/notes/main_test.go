package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type gitResponse struct {
	output string
	err    error
}

type fakeGit map[string]gitResponse

func (f fakeGit) output(args ...string) (string, error) {
	response, ok := f[strings.Join(args, "\x00")]
	if !ok {
		return "", errors.New("unexpected git command: " + strings.Join(args, " "))
	}
	return response.output, response.err
}

func command(args ...string) string {
	return strings.Join(args, "\x00")
}

func missingFile(string) ([]byte, error) {
	return nil, os.ErrNotExist
}

func TestGenerateGroupsHistoryAndUsesCuratedFile(t *testing.T) {
	git := fakeGit{
		command("rev-parse", "--verify", "refs/tags/v1.2.0^{commit}"): {output: "release"},
		command("rev-list", "--parents", "-n", "1", "v1.2.0"):         {output: "release parent"},
		command("tag", "--merged", "v1.2.0^", "--sort=-version:refname", "--list", "v*"): {
			output: "not-semver\nv1.1.0\nv1.0.0",
		},
		command("log", "--first-parent", "--format="+gitLogFormat, "v1.1.0..v1.2.0"): {
			output: "111111111111\x1ffeat(cli): add release command\x1e" +
				"222222222222\x1ffix!: remove broken fallback\x1e" +
				"333333333333\x1fdocs: explain releases\x1e" +
				"444444444444\x1fMerge pull request #42\x1e",
		},
	}
	readFile := func(path string) ([]byte, error) {
		if filepath.ToSlash(path) != ".github/release-notes/v1.2.0.md" {
			t.Fatalf("read path = %q", path)
		}
		return []byte("A curated overview.\n"), nil
	}

	got, err := generate("v1.2.0", git, readFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"A curated overview.",
		"Changes since `v1.1.0`.",
		"### Breaking changes",
		"- remove broken fallback (`2222222`)",
		"### Features",
		"- **cli:** add release command (`1111111`)",
		"### Documentation",
		"### Other changes",
		"- Merge pull request #42 (`4444444`)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notes missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "remove broken fallback") != 1 {
		t.Errorf("breaking change should appear exactly once:\n%s", got)
	}
}

func TestGenerateUsesAnnotatedTagForInitialRelease(t *testing.T) {
	git := fakeGit{
		command("rev-parse", "--verify", "refs/tags/v1.0.0^{commit}"): {output: "release"},
		command("cat-file", "-t", "refs/tags/v1.0.0"):                 {output: "tag"},
		command("for-each-ref", "--format=%(contents)", "refs/tags/v1.0.0"): {
			output: "The first curated release.",
		},
		command("rev-list", "--parents", "-n", "1", "v1.0.0"):                {output: "release"},
		command("log", "--first-parent", "--format="+gitLogFormat, "v1.0.0"): {output: ""},
	}

	got, err := generate("v1.0.0", git, missingFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"The first curated release.", "Initial release.", "No commits are included"} {
		if !strings.Contains(got, want) {
			t.Errorf("notes missing %q:\n%s", want, got)
		}
	}
}

func TestGenerateRequiresCuratedNoteForLightweightTag(t *testing.T) {
	for name, readFile := range map[string]func(string) ([]byte, error){
		"missing file": missingFile,
		"empty file":   func(string) ([]byte, error) { return []byte(" \n"), nil },
	} {
		t.Run(name, func(t *testing.T) {
			git := fakeGit{
				command("rev-parse", "--verify", "refs/tags/v2.0.0^{commit}"): {output: "release"},
				command("cat-file", "-t", "refs/tags/v2.0.0"):                 {output: "commit"},
			}

			_, err := generate("v2.0.0", git, readFile)
			if err == nil || !strings.Contains(err.Error(), "curated release note is required") {
				t.Fatalf("generate error = %v, want required curated note", err)
			}
		})
	}
}

func TestGenerateRequiresNonEmptyAnnotatedTagMessage(t *testing.T) {
	git := fakeGit{
		command("rev-parse", "--verify", "refs/tags/v2.0.0^{commit}"): {output: "release"},
		command("cat-file", "-t", "refs/tags/v2.0.0"):                 {output: "tag"},
		command("for-each-ref", "--format=%(contents)", "refs/tags/v2.0.0"): {
			output: " \n",
		},
	}

	_, err := generate("v2.0.0", git, missingFile)
	if err == nil || !strings.Contains(err.Error(), "curated release note is required") {
		t.Fatalf("generate error = %v, want required curated note", err)
	}
}

func TestGenerateRecognizesBreakingChangeFooters(t *testing.T) {
	git := fakeGit{
		command("rev-parse", "--verify", "refs/tags/v1.2.0^{commit}"): {output: "release"},
		command("rev-list", "--parents", "-n", "1", "v1.2.0"):         {output: "release parent"},
		command("tag", "--merged", "v1.2.0^", "--sort=-version:refname", "--list", "v*"): {
			output: "v1.1.0",
		},
		command("log", "--first-parent", "--format="+gitLogFormat, "v1.1.0..v1.2.0"): {
			output: "111111111111\x1ffeat(api): replace legacy endpoint\n\n" +
				"BREAKING CHANGE: clients must use /v2\x1e" +
				"222222222222\x1ffix(config): reject legacy keys\n\n" +
				"BREAKING-CHANGE: rename old_key to new_key\x1e",
		},
	}
	readFile := func(string) ([]byte, error) {
		return []byte("A curated overview."), nil
	}

	got, err := generate("v1.2.0", git, readFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"### Breaking changes",
		"- **api:** replace legacy endpoint (clients must use /v2) (`1111111`)",
		"- **config:** reject legacy keys (rename old_key to new_key) (`2222222`)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notes missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "### Features") || strings.Contains(got, "### Bug fixes") {
		t.Errorf("footer-marked commits must only appear as breaking changes:\n%s", got)
	}
}

func TestGenerateRejectsInvalidTagBeforeGit(t *testing.T) {
	for _, tag := range []string{"", "1.2.3", "v1.2", "v01.2.3", "v1.2.3-", "v1.2.3-01", "v1.2.3+build"} {
		t.Run(tag, func(t *testing.T) {
			if _, err := generate(tag, fakeGit{}, missingFile); err == nil {
				t.Fatalf("generate(%q) should fail", tag)
			}
		})
	}
}

func TestGenerateAcceptsPrereleaseTag(t *testing.T) {
	git := fakeGit{
		command("rev-parse", "--verify", "refs/tags/v1.3.0-beta.2^{commit}"): {output: "release"},
		command("rev-list", "--parents", "-n", "1", "v1.3.0-beta.2"):         {output: "release parent"},
		command("tag", "--merged", "v1.3.0-beta.2^", "--sort=-version:refname", "--list", "v*"): {
			output: "v1.3.0-beta.1\nv1.2.0",
		},
		command("log", "--first-parent", "--format="+gitLogFormat, "v1.2.0..v1.3.0-beta.2"): {
			output: "111111111111\x1ffeat: preview a feature\x1e",
		},
	}
	readFile := func(path string) ([]byte, error) {
		if filepath.ToSlash(path) != ".github/release-notes/v1.3.0-beta.2.md" {
			t.Fatalf("read path = %q", path)
		}
		return []byte("A beta overview.\n"), nil
	}

	got, err := generate("v1.3.0-beta.2", git, readFile)
	if err != nil {
		t.Fatal(err)
	}
	// previousTag must skip the pre-release candidate v1.3.0-beta.1 and
	// anchor the changelog on the last stable tag, v1.2.0.
	for _, want := range []string{"A beta overview.", "Changes since `v1.2.0`.", "- preview a feature (`1111111`)"} {
		if !strings.Contains(got, want) {
			t.Errorf("notes missing %q:\n%s", want, got)
		}
	}
}

func TestRunWritesOutputFile(t *testing.T) {
	git := fakeGit{
		command("rev-parse", "--verify", "refs/tags/v1.0.0^{commit}"): {output: "release"},
		command("rev-list", "--parents", "-n", "1", "v1.0.0"):         {output: "release"},
		command("log", "--first-parent", "--format="+gitLogFormat, "v1.0.0"): {
			output: "abcdef123456\x1ffeat: ship releases\x1e",
		},
	}
	output := t.TempDir() + "/notes.md"
	var stdout, stderr bytes.Buffer
	readFile := func(string) ([]byte, error) { return []byte("A curated overview."), nil }
	if err := run([]string{"-tag", "v1.0.0", "-output", output}, &stdout, &stderr, git, readFile); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "### Features") {
		t.Fatalf("output does not contain generated changelog:\n%s", data)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunCombinesFeatureNotesIntoOutputFile(t *testing.T) {
	git := fakeGit{
		command("rev-parse", "--verify", "refs/tags/v1.0.0^{commit}"): {output: "release"},
		command("rev-list", "--parents", "-n", "1", "v1.0.0"):         {output: "release"},
		command("log", "--first-parent", "--format="+gitLogFormat, "v1.0.0"): {
			output: "abcdef123456\x1ffeat: ship releases\x1e",
		},
	}
	output := t.TempDir() + "/RELEASE_NOTES.md"
	readFile := func(path string) ([]byte, error) {
		if path == output {
			return []byte(`# Goobers v1.0.0

## Highlights

- No curated highlights supplied.

## DSL feature-support delta

This is the first recorded snapshot.

## Support policy for external consumers

- Pin the release and snapshot.
`), nil
		}
		return []byte("A curated overview."), nil
	}

	var stdout, stderr bytes.Buffer
	err := run(
		[]string{"-tag", "v1.0.0", "-feature-notes", output, "-output", output},
		&stdout,
		&stderr,
		git,
		readFile,
	)
	if err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"A curated overview.",
		"## Changelog",
		"## DSL feature-support delta",
		"## Support policy for external consumers",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("combined notes missing %q:\n%s", want, data)
		}
	}
	if strings.Contains(string(data), "No curated highlights supplied") {
		t.Errorf("combined notes retained generated highlight placeholder:\n%s", data)
	}
}

func TestCombineFeatureNotesRequiresFeatureDelta(t *testing.T) {
	if _, err := combineFeatureNotes("changelog", "not feature notes"); err == nil {
		t.Fatal("combineFeatureNotes should reject notes without a feature delta")
	}
}

// TestBoundReleaseNotes covers the #3341 class: GitHub rejects release bodies
// over 125k characters at PUBLISH time (v0.2.0's publish died there after the
// #3292 backfill grew the delta tables). The bound must trim delta table rows
// at build time, leave short notes byte-identical, keep every heading the
// smoke checks grep for, and error loudly when the overflow is untrimmable.
func TestBoundReleaseNotes(t *testing.T) {
	row := "| `workflow.spec.example.field` | 2.0 | ga | supported | dev |\n"
	head := "# v9.9.9\n\ncurated prose\n\n## DSL feature-support delta\n\n| Feature | DSL version | Feature support | Version support | Since |\n| --- | --- | --- | --- | --- |\n"
	tail := "\n## DSL support-matrix delta\n\n| Version | Change |\n| --- | --- |\n| 2.0 | supported |\n\n## Support policy for external consumers\n\npolicy text\n"

	t.Run("short notes pass unchanged", func(t *testing.T) {
		notes := head + row + tail
		got, err := boundReleaseNotes(notes)
		if err != nil {
			t.Fatalf("boundReleaseNotes = %v, want nil", err)
		}
		if got != notes {
			t.Fatal("short notes must be byte-identical")
		}
	})

	t.Run("oversized notes trim delta rows and keep headings", func(t *testing.T) {
		var sb strings.Builder
		sb.WriteString(head)
		for sb.Len() < maxReleaseNotesLen+40000 {
			sb.WriteString(row)
		}
		sb.WriteString(tail)
		got, err := boundReleaseNotes(sb.String())
		if err != nil {
			t.Fatalf("boundReleaseNotes = %v, want nil", err)
		}
		if len(got) > maxReleaseNotesLen {
			t.Fatalf("len = %d, want <= %d", len(got), maxReleaseNotesLen)
		}
		for _, heading := range []string{"## DSL feature-support delta", "## DSL support-matrix delta", "## Support policy for external consumers"} {
			if !strings.Contains(got, heading) {
				t.Fatalf("heading %q lost in trim", heading)
			}
		}
		if !strings.Contains(got, "rows omitted to fit GitHub's release-body limit") {
			t.Fatal("omission line missing")
		}
		if !strings.Contains(got, "docs/feature-matrix.md") {
			t.Fatal("omission line must point at the complete matrix")
		}
	})

	t.Run("untrimmable overflow errors at build time", func(t *testing.T) {
		notes := "# v9.9.9\n\n" + strings.Repeat("authored prose overflow ", 8000) +
			"\n\n## DSL feature-support delta\n\n(no table)\n"
		if _, err := boundReleaseNotes(notes); err == nil {
			t.Fatal("want an error when the overflow is outside trimmable delta tables")
		}
	})
}
