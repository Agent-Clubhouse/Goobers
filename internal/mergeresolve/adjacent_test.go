package mergeresolve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeAdjacentLineInsertionsPreservesDistinctQuotedEntries(t *testing.T) {
	ancestor := "items = [\n  \"existing\",\n]\n"
	upstream := "items = [\n  \"existing\",\n  \"from-base\",\n]\n"
	incoming := "items = [\n  \"existing\",\n  \"from-pr\",\n]\n"

	merged, ok := MergeAdjacentLineInsertions("items.go", []byte(ancestor), []byte(upstream), []byte(incoming))
	if !ok {
		t.Fatal("MergeAdjacentLineInsertions() rejected comma-terminated quoted entries")
	}
	want := "items = [\n  \"existing\",\n  \"from-base\",\n  \"from-pr\",\n]\n"
	if string(merged) != want {
		t.Fatalf("MergeAdjacentLineInsertions() = %q, want %q", merged, want)
	}
}

func TestMergeAdjacentLineInsertionsRejectsUnsafeCases(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		ancestor string
		upstream string
		incoming string
	}{
		{
			name:     "same addition is ambiguous",
			ancestor: "items:\n  - existing\n",
			upstream: "items:\n  - existing\n  - duplicate\n",
			incoming: "items:\n  - existing\n  - duplicate\n",
		},
		{
			name:     "replacement is overlapping",
			ancestor: "value: old\n",
			upstream: "value: base\n",
			incoming: "value: pr\n",
		},
		{
			name:     "multiple lines are not trivial",
			ancestor: "items:\n  - existing\n",
			upstream: "items:\n  - existing\n  - base-one\n  - base-two\n",
			incoming: "items:\n  - existing\n  - from-pr\n",
		},
		{
			name:     "different indentation is structural",
			ancestor: "items:\n  - existing\n",
			upstream: "items:\n  - existing\n  - base\n",
			incoming: "items:\n  - existing\n    - nested\n",
		},
		{
			name:     "repeated context has no unique insertion point",
			ancestor: "  - item\n",
			upstream: "  - item\n  - item\n",
			incoming: "  - item\n  - other\n",
		},
		{
			name:     "unterminated additions would concatenate",
			ancestor: "items:\n  - existing\n",
			upstream: "items:\n  - existing\n  - base",
			incoming: "items:\n  - existing\n  - pr",
		},
		{
			name:     "python function body is structural",
			path:     "logic.py",
			ancestor: "def f():\n    - existing\n",
			upstream: "def f():\n    - existing\n    - from_base\n",
			incoming: "def f():\n    - existing\n    - from_pr\n",
		},
		{
			name:     "malformed YAML is not a verified list",
			ancestor: "items:\n  - existing\nbroken: [\n",
			upstream: "items:\n  - existing\n  - from-base\nbroken: [\n",
			incoming: "items:\n  - existing\n  - from-pr\nbroken: [\n",
		},
		{
			name:     "executable block is structural",
			ancestor: "func run() {\n\talreadyRunning()\n}\n",
			upstream: "func run() {\n\talreadyRunning()\n\tfromBase()\n}\n",
			incoming: "func run() {\n\talreadyRunning()\n\tfromPR()\n}\n",
		},
		{
			name:     "comma terminated calls are structural",
			ancestor: "run(\n\texisting(),\n)\n",
			upstream: "run(\n\texisting(),\n\tfromBase(),\n)\n",
			incoming: "run(\n\texisting(),\n\tfromPR(),\n)\n",
		},
		{
			name:     "quoted function arguments are structural",
			ancestor: "run(\n  \"existing\",\n)\n",
			upstream: "run(\n  \"existing\",\n  \"from-base\",\n)\n",
			incoming: "run(\n  \"existing\",\n  \"from-pr\",\n)\n",
		},
		{
			name:     "quoted entries without separators are ambiguous",
			ancestor: "items = [\n  \"existing\",\n]\n",
			upstream: "items = [\n  \"existing\",\n  \"from-base\"\n]\n",
			incoming: "items = [\n  \"existing\",\n  \"from-pr\"\n]\n",
		},
		{
			name:     "quoted map entries are structural",
			ancestor: "items = {\n  \"existing\": \"value\",\n}\n",
			upstream: "items = {\n  \"existing\": \"value\",\n  \"shared\": \"base\",\n}\n",
			incoming: "items = {\n  \"existing\": \"value\",\n  \"shared\": \"pr\",\n}\n",
		},
		{
			name:     "isolated list-shaped lines are ambiguous",
			ancestor: "heading\nfooter\n",
			upstream: "heading\n- from-base\nfooter\n",
			incoming: "heading\n- from-pr\nfooter\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.path
			if path == "" {
				path = "items.yaml"
			}
			if merged, ok := MergeAdjacentLineInsertions(path, []byte(tt.ancestor), []byte(tt.upstream), []byte(tt.incoming)); ok {
				t.Fatalf("MergeAdjacentLineInsertions() = %q, true; want rejection", merged)
			}
		})
	}
}

func TestHasStandardTextMergeAttributesRejectsContentTransforms(t *testing.T) {
	for _, attributes := range []string{
		"items.yaml filter=custom\n",
		"items.yaml ident\n",
		"items.yaml working-tree-encoding=UTF-16LE\n",
	} {
		t.Run(strings.Fields(attributes)[1], func(t *testing.T) {
			dir := t.TempDir()
			mustGit(t, dir, "init", "-q")
			if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte(attributes), 0o644); err != nil {
				t.Fatalf("write attributes: %v", err)
			}
			standard, err := HasStandardTextMergeAttributes(testGit(t, dir), "items.yaml")
			if err != nil {
				t.Fatalf("HasStandardTextMergeAttributes: %v", err)
			}
			if standard {
				t.Fatalf("attributes %q accepted, want content transform rejected", attributes)
			}
		})
	}
}

func TestHasMarkerListContainer(t *testing.T) {
	tests := []struct {
		name     string
		ancestor []string
		insertAt int
		indent   string
		want     bool
	}{
		{"empty indent", []string{"items:\n", "- a\n"}, 1, "", false},
		{"under key", []string{"items:\n", "  - a\n"}, 1, "  ", true},
		{"after sibling entry", []string{"items:\n", "  - a\n", "  - b\n"}, 2, "  ", true},
		{"blank lines skipped", []string{"items:\n", "\n", "  - a\n"}, 2, "  ", true},
		{"nested key", []string{"a:\n", "  b:\n", "    - x\n"}, 2, "    ", true},
		{"opener without colon", []string{"intro\n", "  - a\n"}, 1, "  ", false},
		{"no preceding lines", []string{"  - a\n"}, 0, "  ", false},
		{"only blanks before", []string{"\n", "  - a\n"}, 1, "  ", false},
		{"indent mismatch tabs vs spaces", []string{"\titems:\n", "  - a\n"}, 1, "  ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasMarkerListContainer(tt.ancestor, tt.insertAt, tt.indent); got != tt.want {
				t.Fatalf("hasMarkerListContainer() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasQuotedListContainer(t *testing.T) {
	tests := []struct {
		name     string
		ancestor []string
		insertAt int
		indent   string
		want     bool
	}{
		{"bracketed", []string{"x = [\n", "  \"a\",\n", "]\n"}, 2, "  ", true},
		{"closer with comma", []string{"x = [\n", "  \"a\",\n", "],\n"}, 2, "  ", true},
		{"insert at start", []string{"x = [\n", "  \"a\",\n", "]\n"}, 1, "  ", true},
		{"blank lines skipped", []string{"x = [\n", "\n", "  \"a\",\n", "\n", "]\n"}, 3, "  ", true},
		{"no opener", []string{"  \"a\",\n", "]\n"}, 1, "  ", false},
		{"non-bracket opener", []string{"x = {\n", "  \"a\",\n", "}\n"}, 2, "  ", false},
		{"no closer", []string{"x = [\n", "  \"a\",\n"}, 2, "  ", false},
		{"wrong closer", []string{"x = [\n", "  \"a\",\n", "}\n"}, 2, "  ", false},
		{"closer indent differs", []string{"x = [\n", "  \"a\",\n", " ]\n"}, 2, "  ", false},
		{"nested opener indent", []string{"  x = [\n", "    \"a\",\n", "  ]\n"}, 2, "    ", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasQuotedListContainer(tt.ancestor, tt.insertAt, tt.indent); got != tt.want {
				t.Fatalf("hasQuotedListContainer() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMergeAdjacentLineInsertionsListContainerOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		ancestor  string
		upstream  string
		incoming  string
		wantMerge bool
	}{
		{
			"yaml list merges", "a.yaml",
			"items:\n  - a\n", "items:\n  - a\n  - b\n", "items:\n  - a\n  - c\n", true,
		},
		{
			"top-level yaml marker not merged", "a.yaml",
			"- a\n", "- a\n- b\n", "- a\n- c\n", false,
		},
		{
			"quoted list merges", "a.go",
			"x = [\n  \"a\",\n]\n", "x = [\n  \"a\",\n  \"b\",\n]\n", "x = [\n  \"a\",\n  \"c\",\n]\n", true,
		},
		{
			"quoted entries in braces not merged", "a.go",
			"x = {\n  \"a\",\n}\n", "x = {\n  \"a\",\n  \"b\",\n}\n", "x = {\n  \"a\",\n  \"c\",\n}\n", false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := MergeAdjacentLineInsertions(tt.path, []byte(tt.ancestor), []byte(tt.upstream), []byte(tt.incoming))
			if ok != tt.wantMerge {
				t.Fatalf("MergeAdjacentLineInsertions() ok = %v, want %v", ok, tt.wantMerge)
			}
		})
	}
}
