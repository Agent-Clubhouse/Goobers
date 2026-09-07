package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDocumentAcceptsEnumWithMarkdownAndDetail(t *testing.T) {
	t.Parallel()
	path := writeDocument(t, "# Design\n\n> **Status:** Implemented — GA in #1939\n> Delivered-by: #1939\n")

	doc, err := parseDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != "implemented" {
		t.Errorf("status = %q, want implemented", doc.Status)
	}
	if len(doc.DeliveredBy) != 1 || doc.DeliveredBy[0] != "#1939" {
		t.Errorf("deliveredBy = %v, want [#1939]", doc.DeliveredBy)
	}
	if doc.Title != "Design" {
		t.Errorf("title = %q", doc.Title)
	}
}

func TestParseDocumentRejectsMissingMarker(t *testing.T) {
	t.Parallel()
	path := writeDocument(t, "# Design\n\nNo status here.\n")

	_, err := parseDocument(path)
	if err == nil || !strings.Contains(err.Error(), "missing Status marker") {
		t.Fatalf("error = %v, want missing marker", err)
	}
}

func TestValidateRejectsUnknownStatus(t *testing.T) {
	t.Parallel()
	docs := []document{{Path: "docs/design/x.md", Status: "proposed"}}

	problems := validate(docs)
	if len(problems) != 1 || !strings.Contains(problems[0], `unknown status "proposed"`) {
		t.Fatalf("problems = %v, want unknown status", problems)
	}
}

// The point of #4518: `implemented` used to be a word anyone could type. It now
// has to name what landed.
func TestValidateRejectsImplementedWithoutDeliveryEvidence(t *testing.T) {
	t.Parallel()
	docs := []document{{Path: "docs/design/x.md", Status: "implemented"}}

	problems := validate(docs)
	if len(problems) != 1 || !strings.Contains(problems[0], "Delivered-by") {
		t.Fatalf("problems = %v, want a delivery-evidence complaint", problems)
	}

	docs[0].DeliveredBy = []string{"#123"}
	if problems := validate(docs); len(problems) != 0 {
		t.Fatalf("problems = %v, want none once delivery is named", problems)
	}
}

// A partial implementation must be representable without overstating it: list
// what did land, and keep the honest status.
func TestValidateAcceptsPartialDeliveryUnderApproved(t *testing.T) {
	t.Parallel()
	docs := []document{{Path: "docs/design/x.md", Status: "approved", DeliveredBy: []string{"#1", "#2"}}}

	if problems := validate(docs); len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
}

func TestValidateRejectsSupersededWithoutForwardPointer(t *testing.T) {
	t.Parallel()
	docs := []document{{Path: "docs/design/x.md", Status: "superseded"}}

	problems := validate(docs)
	if len(problems) != 1 || !strings.Contains(problems[0], "Superseded-by") {
		t.Fatalf("problems = %v, want a forward-pointer complaint", problems)
	}
}

// The audit's concrete failure: goobernetes-architecture.md declared two
// documents superseded and neither gained a forward pointer, so a newer design
// was written against the obsolete one.
func TestValidateRejectsOneSidedSupersession(t *testing.T) {
	t.Parallel()
	docs := []document{
		{Path: "docs/design/new.md", Status: "approved", Supersedes: []string{"docs/design/old.md"}},
		{Path: "docs/design/old.md", Status: "historical"},
	}

	problems := validate(docs)
	if len(problems) != 1 || !strings.Contains(problems[0], "reciprocal") {
		t.Fatalf("problems = %v, want a reciprocity complaint", problems)
	}

	docs[1].SupersededBy = []string{"docs/design/new.md"}
	if problems := validate(docs); len(problems) != 0 {
		t.Fatalf("problems = %v, want none once reciprocal", problems)
	}
}

func TestValidateRejectsSelfSupersession(t *testing.T) {
	t.Parallel()
	docs := []document{{Path: "docs/design/x.md", Status: "superseded", SupersededBy: []string{"docs/design/x.md"}}}

	problems := validate(docs)
	if len(problems) != 1 || !strings.Contains(problems[0], "the document itself") {
		t.Fatalf("problems = %v, want a self-supersession complaint", problems)
	}
}

// A design superseded by a requirements spec is a real case. Reciprocity cannot
// be asked of a document outside this corpus, but the target must exist.
func TestValidateAllowsSupersessionOutsideTheCorpusWhenTheFileExists(t *testing.T) {
	// Resolves out-of-corpus targets against the real tree, so it must run
	// from the repository root the way the command does. t.Chdir forbids
	// t.Parallel.
	t.Chdir(repoRoot(t))
	docs := []document{{
		Path: "docs/design/x.md", Status: "superseded",
		SupersededBy: []string{"docs/requirements/pr-lifecycle.md"},
	}}

	if problems := validate(docs); len(problems) != 0 {
		t.Fatalf("problems = %v, want none for an existing out-of-corpus target", problems)
	}

	docs[0].SupersededBy = []string{"docs/requirements/does-not-exist.md"}
	problems := validate(docs)
	if len(problems) != 1 || !strings.Contains(problems[0], "does not exist") {
		t.Fatalf("problems = %v, want a dangling-pointer complaint", problems)
	}
}

func TestValidateRejectsUndisclaimedMachineLocalCitation(t *testing.T) {
	t.Parallel()
	docs := []document{{
		Path: "docs/design/x.md", Status: "draft",
		MachineLocalPaths: []string{"~/source/Reviews/finding.md"},
	}}

	problems := validate(docs)
	if len(problems) != 1 || !strings.Contains(problems[0], externalEvidenceDisclaimer) {
		t.Fatalf("problems = %v, want an external-evidence complaint", problems)
	}

	docs[0].DisclaimsExternalEvidence = true
	if problems := validate(docs); len(problems) != 0 {
		t.Fatalf("problems = %v, want none once disclaimed", problems)
	}
}

// A product path that happens to live under a home directory names the same
// location on every machine and must not be flagged.
func TestIsMachineLocalCitationExcludesProductPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"~/source/Goobers-Review/findings.md", true},
		{"/Users/someone/source/checkout", true},
		{"/home/someone/instances", true},
		{"~/.copilot/mcp-config.json", false},
		{`C:\Users\ContainerUser\AppData\Local\Temp`, false},
		{"/home/runner/work", false},
	} {
		if got := isMachineLocalCitation(tc.path); got != tc.want {
			t.Errorf("isMachineLocalCitation(%q) = %t, want %t", tc.path, got, tc.want)
		}
	}
}

// The disclaimer must survive reflowing the paragraph that carries it.
func TestDisclaimerMatchIsWrapInsensitive(t *testing.T) {
	t.Parallel()
	path := writeDocument(t, "# Design\n\n> Status: draft\n\nCited `~/source/x` — **not reproducible from\nthis repository**.\n")

	doc, err := parseDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if !doc.DisclaimsExternalEvidence {
		t.Error("wrapped disclaimer was not recognised")
	}
}

func TestRenderIndexListsEveryDocument(t *testing.T) {
	t.Parallel()
	index := renderIndex([]document{
		{Path: "docs/adr/0001-x.md", Title: "ADR 0001", Status: "approved"},
		{Path: "docs/design/a.md", Title: "Alpha", Status: "implemented", DeliveredBy: []string{"#1"}},
	})

	for _, want := range []string{"ADR 0001", "Alpha", "`implemented`", "#1", "**Total** | **2**"} {
		if !strings.Contains(index, want) {
			t.Errorf("index does not mention %q", want)
		}
	}
}

func TestLoadDocumentsSkipsTheGeneratedIndex(t *testing.T) {
	t.Chdir(repoRoot(t))
	// The index has no Status marker of its own; loading it would report a
	// spurious problem on every run.
	docs, problems := loadDocuments(designRoot)
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	for _, doc := range docs {
		if doc.Path == indexPath {
			t.Fatal("the generated index must not be treated as a design document")
		}
	}
}

// repoRoot returns the repository root, since these tests live two directories
// below it and the command is always run from the root.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func writeDocument(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "document.md")
	writeDocumentAt(t, path, content)
	return path
}

func writeDocumentAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
