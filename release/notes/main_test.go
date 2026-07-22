package main

import (
	"bytes"
	"errors"
	"os"
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
		command("log", "--first-parent", "--format=%H%x09%s", "v1.1.0..v1.2.0"): {
			output: strings.Join([]string{
				"111111111111\tfeat(cli): add release command",
				"222222222222\tfix!: remove broken fallback",
				"333333333333\tdocs: explain releases",
				"444444444444\tMerge pull request #42",
			}, "\n"),
		},
	}
	readFile := func(path string) ([]byte, error) {
		if path != ".github/release-notes/v1.2.0.md" {
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
		command("rev-list", "--parents", "-n", "1", "v1.0.0"):           {output: "release"},
		command("log", "--first-parent", "--format=%H%x09%s", "v1.0.0"): {output: ""},
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

func TestGenerateUsesFallbackForLightweightTag(t *testing.T) {
	git := fakeGit{
		command("rev-parse", "--verify", "refs/tags/v2.0.0^{commit}"): {output: "release"},
		command("cat-file", "-t", "refs/tags/v2.0.0"):                 {output: "commit"},
		command("rev-list", "--parents", "-n", "1", "v2.0.0"):         {output: "release parent"},
		command("tag", "--merged", "v2.0.0^", "--sort=-version:refname", "--list", "v*"): {
			output: "",
		},
		command("log", "--first-parent", "--format=%H%x09%s", "v2.0.0"): {
			output: "abcdef123456\tsecurity(auth): rotate token flow",
		},
	}

	got, err := generate("v2.0.0", git, missingFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"This release includes the changes listed below.",
		"### Other changes",
		"- **auth:** rotate token flow (`abcdef1`)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notes missing %q:\n%s", want, got)
		}
	}
}

func TestGenerateRejectsNonStableTagBeforeGit(t *testing.T) {
	for _, tag := range []string{"", "1.2.3", "v1.2", "v1.2.3-rc.1", "v01.2.3"} {
		t.Run(tag, func(t *testing.T) {
			if _, err := generate(tag, fakeGit{}, missingFile); err == nil {
				t.Fatalf("generate(%q) should fail", tag)
			}
		})
	}
}

func TestRunWritesOutputFile(t *testing.T) {
	git := fakeGit{
		command("rev-parse", "--verify", "refs/tags/v1.0.0^{commit}"): {output: "release"},
		command("cat-file", "-t", "refs/tags/v1.0.0"):                 {output: "commit"},
		command("rev-list", "--parents", "-n", "1", "v1.0.0"):         {output: "release"},
		command("log", "--first-parent", "--format=%H%x09%s", "v1.0.0"): {
			output: "abcdef123456\tfeat: ship releases",
		},
	}
	output := t.TempDir() + "/notes.md"
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-tag", "v1.0.0", "-output", output}, &stdout, &stderr, git, missingFile); err != nil {
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
