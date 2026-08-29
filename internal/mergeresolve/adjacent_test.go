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
