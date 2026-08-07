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

func fixtureRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, root, "docs/index.md", "# Docs\n")
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
