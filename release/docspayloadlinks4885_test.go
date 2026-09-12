package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePayloadDoc(t *testing.T, payloadDir, rel, body string) {
	t.Helper()
	path := filepath.Join(payloadDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readPayloadDoc(t *testing.T, payloadDir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(payloadDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The defect in #4885: only the root README was pinned, so every other
// markdown file in the payload shipped links that resolve in the repository
// and dangle in the archive.
func TestPinsLinksOutsideTheRootReadme(t *testing.T) {
	payloadDir := t.TempDir()
	writePayloadDoc(t, payloadDir, "docs/guides/supervision.md",
		"See [the requirement](../requirements/instance.md) and [the design](../../internal/design.md).\n")
	writePayloadDoc(t, payloadDir, "docs/requirements/instance.md", "# Instance\n")

	if err := pinUnresolvablePayloadLinks(payloadDir, "v0.4.0-rc.3"); err != nil {
		t.Fatalf("pinUnresolvablePayloadLinks: %v", err)
	}
	got := readPayloadDoc(t, payloadDir, "docs/guides/supervision.md")

	// Present in the archive: the relative link is left alone. Resolution is
	// relative to the LINKING document's directory, which is the whole
	// difficulty a root-only pin never had to face.
	if !strings.Contains(got, "](../requirements/instance.md)") {
		t.Errorf("rewrote a link the payload can satisfy:\n%s", got)
	}
	// Absent from the archive: pinned to the tag, at the path it names from
	// the repository root, not from the payload root.
	if !strings.Contains(got, "](https://github.com/Agent-Clubhouse/Goobers/blob/v0.4.0-rc.3/internal/design.md)") {
		t.Errorf("did not pin a link that escapes the payload:\n%s", got)
	}
}

// Extending the pin across the documentation tree brings it into contact with
// fenced markdown samples for the first time. Rewriting a link inside one
// would corrupt the example rather than fix anything.
func TestLeavesFencedCodeBlocksAlone(t *testing.T) {
	payloadDir := t.TempDir()
	body := "Intro [real](missing.md).\n\n```markdown\n[sample](also-missing.md)\n```\n\nOutro.\n"
	writePayloadDoc(t, payloadDir, "docs/authoring.md", body)

	if err := pinUnresolvablePayloadLinks(payloadDir, "v0.4.0-rc.3"); err != nil {
		t.Fatalf("pinUnresolvablePayloadLinks: %v", err)
	}
	got := readPayloadDoc(t, payloadDir, "docs/authoring.md")

	if !strings.Contains(got, "](https://github.com/Agent-Clubhouse/Goobers/blob/v0.4.0-rc.3/docs/missing.md)") {
		t.Errorf("prose link was not pinned:\n%s", got)
	}
	if !strings.Contains(got, "[sample](also-missing.md)") {
		t.Errorf("rewrote a link inside a fenced sample:\n%s", got)
	}

	// The guard must agree with the pin, or staging would fail on a sample it
	// deliberately refused to rewrite.
	broken, err := unresolvablePayloadLinks(payloadDir)
	if err != nil {
		t.Fatalf("unresolvablePayloadLinks: %v", err)
	}
	if len(broken) != 0 {
		t.Errorf("unresolvable links after pinning = %v, want none", broken)
	}
}

func TestResolvePayloadLink(t *testing.T) {
	payloadDir := t.TempDir()
	writePayloadDoc(t, payloadDir, "docs/reference/cli.md", "# CLI\n")
	writePayloadDoc(t, payloadDir, "README.md", "# Goobers\n")

	tests := []struct {
		name, rel, target, want string
		resolves                bool
	}{
		{name: "sibling", rel: "docs/reference/index.md", target: "cli.md", want: "docs/reference/cli.md", resolves: true},
		{name: "parent hop", rel: "docs/guides/x.md", target: "../reference/cli.md", want: "docs/reference/cli.md", resolves: true},
		{name: "root file from nested", rel: "docs/guides/x.md", target: "../../README.md", want: "README.md", resolves: true},
		{name: "missing", rel: "docs/guides/x.md", target: "../reference/gone.md", want: "docs/reference/gone.md"},
		{name: "escapes payload", rel: "docs/x.md", target: "../../internal/y.go", want: "internal/y.go"},
		{name: "root-absolute", rel: "docs/x.md", target: "/README.md", want: "README.md", resolves: true},
		{name: "external", rel: "docs/x.md", target: "https://example.com", want: "https://example.com", resolves: true},
		{name: "mailto", rel: "docs/x.md", target: "mailto:a@b.c", want: "mailto:a@b.c", resolves: true},
		{name: "same-document anchor", rel: "docs/x.md", target: "#section", want: "#section", resolves: true},
		{name: "fragment kept on a miss", rel: "docs/x.md", target: "gone.md#part", want: "docs/gone.md#part"},
		{name: "fragment on a hit", rel: "docs/x.md", target: "reference/cli.md#usage", want: "docs/reference/cli.md#usage", resolves: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, resolves := resolvePayloadLink(payloadDir, test.rel, test.target)
			if got != test.want || resolves != test.resolves {
				t.Errorf("resolvePayloadLink(%q, %q) = (%q, %t), want (%q, %t)",
					test.rel, test.target, got, resolves, test.want, test.resolves)
			}
		})
	}
}
