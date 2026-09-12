package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRepositoryValidatesPathsAndGitHubHeadingAnchors(t *testing.T) {
	t.Parallel()
	root := fixtureRepository(t)
	writeFixture(t, root, "docs/guide.md", "# Setup & Run\n\n## Repeat\n## Repeat\n\nSetext heading\n--------------\n")
	writeFixture(t, root, "README.md", strings.Join([]string{
		"# Home",
		"[guide](docs/guide.md#setup--run)",
		"[duplicate](docs/guide.md#repeat-1)",
		"[setext](docs/guide.md#setext-heading)",
		"[directory](docs/)",
		"[external](https://example.com/missing#anchor)",
	}, "\n"))

	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %+v", violations)
	}
}

func TestCheckRepositoryReportsFileLineAndTarget(t *testing.T) {
	t.Parallel()
	root := fixtureRepository(t)
	writeFixture(t, root, "docs/guide.md", "# Existing\n")
	writeFixture(t, root, "README.md", "# Home\n[missing file](docs/missing.md)\n[missing anchor](docs/guide.md#missing)\n")

	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 2 {
		t.Fatalf("violations = %+v, want two", violations)
	}
	if violations[0].Path != "README.md" || violations[0].Line != 2 || violations[0].Target != "docs/missing.md" {
		t.Fatalf("first violation = %+v", violations[0])
	}
	if violations[1].Path != "README.md" || violations[1].Line != 3 || violations[1].Target != "docs/guide.md#missing" {
		t.Fatalf("second violation = %+v", violations[1])
	}
}

func TestCheckRepositoryIgnoresLinksInCodeFences(t *testing.T) {
	t.Parallel()
	root := fixtureRepository(t)
	writeFixture(t, root, "docs/guide.md", "```markdown\n[example](missing.md)\n# Not rendered\n```\n")

	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %+v", violations)
	}
}

func TestCheckRepositoryUsesRenderedMarkdownLinks(t *testing.T) {
	t.Parallel()
	root := fixtureRepository(t)
	writeFixture(t, root, "README.md", strings.Join([]string{
		"# Root",
		"`[inline example](docs/missing-inline.md)`",
		`\[escaped example](docs/missing-escaped.md)`,
		"[multiline](",
		"  docs/missing-multiline.md",
		")",
	}, "\n"))

	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %+v, want only the rendered multiline link", violations)
	}
	if violations[0].Line != 4 || violations[0].Target != "docs/missing-multiline.md" {
		t.Fatalf("violation = %+v", violations[0])
	}
}

func TestCheckRepositoryUsesRenderedHeadingTextAndGlobalSlugUniqueness(t *testing.T) {
	t.Parallel()
	root := fixtureRepository(t)
	writeFixture(t, root, "docs/guide.md", strings.Join([]string{
		"# *Styled* `heading`",
		"multiline",
		"setext",
		"------",
		"# foo",
		"# foo-1",
		"# foo",
	}, "\n"))
	writeFixture(t, root, "README.md", strings.Join([]string{
		"# Root",
		"[markup](docs/guide.md#styled-heading)",
		"[multiline](docs/guide.md#multiline-setext)",
		"[collision](docs/guide.md#foo-2)",
	}, "\n"))

	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %+v", violations)
	}
}

func TestCheckRepositoryReportsLinesForEmptyLinkLabels(t *testing.T) {
	t.Parallel()
	root := fixtureRepository(t)
	writeFixture(t, root, "README.md", strings.Join([]string{
		"# Root",
		"",
		"Intro",
		"[](docs/missing-empty.md)",
		"![](docs/missing-image.png)",
	}, "\n"))

	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 2 {
		t.Fatalf("violations = %+v, want two", violations)
	}
	if violations[0].Line != 4 || violations[0].Target != "docs/missing-empty.md" {
		t.Fatalf("first violation = %+v", violations[0])
	}
	if violations[1].Line != 5 || violations[1].Target != "docs/missing-image.png" {
		t.Fatalf("second violation = %+v", violations[1])
	}
}

func TestCheckRepositoryReportsLinesForNonTextLinkLabels(t *testing.T) {
	t.Parallel()
	root := fixtureRepository(t)
	writeFixture(t, root, "README.md", strings.Join([]string{
		"# Root",
		"",
		"Intro paragraph",
		"[<span></span>](docs/missing-html-label.md)",
	}, "\n"))

	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %+v, want one", violations)
	}
	if violations[0].Line != 4 || violations[0].Target != "docs/missing-html-label.md" {
		t.Fatalf("violation = %+v", violations[0])
	}
}

func TestCheckRepositoryValidatesReferenceTargetsAndHTMLAnchors(t *testing.T) {
	t.Parallel()
	root := fixtureRepository(t)
	writeFixture(t, root, "docs/guide.md", "<a id=\"stable-anchor\"></a>\n")
	writeFixture(t, root, "README.md", "[guide][guide]\n\n[guide]: docs/guide.md#stable-anchor \"Guide\"\n")

	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %+v", violations)
	}
}

func TestCheckRepositoryDecodesRelativeURLsOnce(t *testing.T) {
	t.Parallel()
	root := fixtureRepository(t)
	writeFixture(t, root, "docs/space name.md", "<a id=\"percent%anchor\"></a>\n")
	writeFixture(t, root, "docs/literal%20escape.md", "# Literal escape\n")
	writeFixture(t, root, "README.md", strings.Join([]string{
		"# Root",
		"[encoded path and fragment](docs/space%20name.md#percent%25anchor)",
		"[literal percent escape](docs/literal%2520escape.md)",
	}, "\n"))

	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %+v", violations)
	}
}

func TestRepositoryMarkdownLinksResolve(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("repository markdown link violations: %+v", violations)
	}
}

func TestRepositoryGuidesIndexContainsEveryGuideExactlyOnce(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	guides, err := loadGuides(root)
	if err != nil {
		t.Fatalf("loadGuides: %v", err)
	}
	index, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(guidesIndexPath)))
	if err != nil {
		t.Fatalf("read guides index: %v", err)
	}
	for _, current := range guides {
		link := "(" + strings.TrimPrefix(current.path, guidesRoot+"/") + ")"
		if count := strings.Count(string(index), link); count != 1 {
			t.Errorf("%s link appears %d times in generated index, want exactly once", current.path, count)
		}
	}
}

func TestRenderGuidesIndexListsEveryGuideOnceDeterministically(t *testing.T) {
	t.Parallel()
	root := fixtureRepository(t)
	writeFixture(t, root, "docs/guides/z-last.md", "# Zeta ] operations\n")
	writeFixture(t, root, "docs/guides/a-first.md", "# Alpha *setup*\n")

	guides, err := loadGuides(root)
	if err != nil {
		t.Fatalf("loadGuides: %v", err)
	}
	got := renderGuidesIndex(guides)
	want := "# Guides\n\n" +
		"<!-- Generated by `go run ./test/markdownlinks -write`. Do not edit by hand. -->\n\n" +
		"Every maintained guide in this repository. The `markdown-links` check regenerates\n" +
		"this index and fails on drift, so adding, removing, renaming, or retitling a guide\n" +
		"cannot leave it outside the documented navigation surfaces.\n\n" +
		"- [Alpha setup](a-first.md)\n" +
		"- [Zeta \\] operations](z-last.md)\n"
	if got != want {
		t.Fatalf("renderGuidesIndex =\n%s\nwant exact deterministic index =\n%s", got, want)
	}
	for _, link := range []string{"(a-first.md)", "(z-last.md)"} {
		if count := strings.Count(got, link); count != 1 {
			t.Errorf("link %s appears %d times, want exactly once", link, count)
		}
	}
}

func TestCheckRepositoryReportsGuidesIndexDriftAndUnreachableGuide(t *testing.T) {
	t.Parallel()
	root := fixtureRepository(t)
	writeFixture(t, root, "docs/guides/orphan.md", "# Orphaned operations\n")

	violations, err := checkRepository(root)
	if err != nil {
		t.Fatalf("checkRepository: %v", err)
	}
	if len(violations) != 2 {
		t.Fatalf("violations = %+v, want index drift and unreachable-guide failures", violations)
	}
	reasons := violations[0].Reason + "\n" + violations[1].Reason
	for _, want := range []string{"generated guides index is out of date", "guide is unreachable"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("violations = %+v, want reason containing %q", violations, want)
		}
	}
}

func TestGuideReachabilityRecognizesIndexCLIAndManSurfaces(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		surface  string
		contents string
	}{
		{
			name:     "generated index link",
			surface:  guidesIndexPath,
			contents: "# Guides\n\n- [Operations](operations.md)\n",
		},
		{
			name:     "docs cli reference",
			surface:  "docs/cli/extra.md",
			contents: "See docs/guides/operations.md for details.\n",
		},
		{
			name:     "man page reference",
			surface:  "docs/man/goobers-extra.1",
			contents: "See docs/guides/operations.md for details.\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := fixtureRepository(t)
			writeFixture(t, root, "docs/guides/operations.md", "# Operations\n")
			writeFixture(t, root, test.surface, test.contents)
			documents := make(map[string]document)
			if strings.HasSuffix(test.surface, ".md") {
				parsed, err := parseDocument(filepath.Join(root, filepath.FromSlash(test.surface)), test.surface)
				if err != nil {
					t.Fatalf("parse surface: %v", err)
				}
				documents[test.surface] = parsed
			}
			reachable, err := reachableGuides(root, documents)
			if err != nil {
				t.Fatalf("reachableGuides: %v", err)
			}
			if !reachable["docs/guides/operations.md"] {
				t.Fatalf("reachable = %+v, want operations guide reached from %s", reachable, test.surface)
			}
		})
	}
}

func TestLoadGuidesRequiresLevelOneTitle(t *testing.T) {
	t.Parallel()
	root := fixtureRepository(t)
	writeFixture(t, root, "docs/guides/untitled.md", "An introduction without a title.\n")
	if _, err := loadGuides(root); err == nil || !strings.Contains(err.Error(), "needs a level-one title") {
		t.Fatalf("loadGuides error = %v, want missing-title failure", err)
	}
}

func fixtureRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, root, "docs/index.md", "# Docs\n")
	writeFixture(t, root, guidesIndexPath, renderGuidesIndex(nil))
	writeFixture(t, root, "README.md", "# Root\n")
	writeFixture(t, root, "reference-workflows/README.md", "# Workflows\n")
	writeFixture(t, root, "examples/README.md", "# Examples\n")
	return root
}

func writeFixture(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
