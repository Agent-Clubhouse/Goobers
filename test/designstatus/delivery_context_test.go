package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
)

func TestDeliveryContextRejectsIncompleteOrAmbiguousInput(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, input := range []string{
		`{}`, `{"baseRevision":"--help","closingIssues":[]}`,
		`{"baseRevision":"` + sha + `"}`,
		`{"baseRevision":"` + sha + `","closingIssues":null}`,
		`{"baseRevision":"` + sha + `","closingIssues":["other/repo#1"]}`,
		`{"baseRevision":"` + sha + `","closingIssues":[],"bypass":true}`,
		`{"baseRevision":"` + sha + `","closingIssues":[]} {}`,
	} {
		if _, err := parseDeliveryContext([]byte(input)); err == nil {
			t.Fatalf("invalid context accepted: %s", input)
		}
	}
	if _, err := parseDeliveryContext([]byte(`{"baseRevision":"` + sha + `","closingIssues":[]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryContextReadsBaseGitTreeToPreventDeletionBypass(t *testing.T) {
	t.Chdir(t.TempDir())
	git := func(args ...string) string {
		t.Helper()
		out, err := testgit.Command(args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	if err := os.MkdirAll(designRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(designRoot, "tracked.md")
	if err := os.WriteFile(file, []byte("# Tracked design\n> Status: approved\n> Tracking: #42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", file)
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "base")
	base := git("rev-parse", "HEAD")
	data, err := json.Marshal(deliveryContext{BaseRevision: base, ClosingIssues: []string{"#42"}})
	if err != nil {
		t.Fatal(err)
	}
	contextPath := "context.json"
	if err := os.WriteFile(contextPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	problems := checkDeliveryContext(contextPath, nil)
	if !strings.Contains(strings.Join(problems, "\n"), "retaining and updating") {
		t.Fatalf("deleted design bypassed real base comparison: %v", problems)
	}
	updated := document{Path: filepath.ToSlash(file), Status: "implemented", DeliveredBy: []string{"#42"}, ScopeDelta: "none", Verified: base + " (2026-09-07)"}
	if problems := checkDeliveryContext(contextPath, []document{updated}); len(problems) != 0 {
		t.Fatalf("valid delivery rejected: %v", problems)
	}
}
