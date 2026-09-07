package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStageReleaseRootFilesCarriesLicenseAndSecurity is #4268's licence pin.
//
// All five published v0.4.0-beta.2 archives redistributed an MIT-licensed
// binary with no licence text inside them: `tar -tzf ... | grep -ci license`
// returned 0 for every .tar.gz, and the Windows .zip too.
func TestStageReleaseRootFilesCarriesLicenseAndSecurity(t *testing.T) {
	repoRoot := t.TempDir()
	payloadDir := t.TempDir()
	for _, name := range releaseRootFiles {
		if err := os.WriteFile(filepath.Join(repoRoot, name), []byte("contents of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := stageReleaseRootFiles(repoRoot, payloadDir); err != nil {
		t.Fatalf("stageReleaseRootFiles: %v", err)
	}
	for _, name := range releaseRootFiles {
		data, err := os.ReadFile(filepath.Join(payloadDir, name))
		if err != nil {
			t.Fatalf("%s missing from the release payload: %v", name, err)
		}
		if string(data) != "contents of "+name {
			t.Fatalf("%s = %q, want the repository copy", name, data)
		}
	}
}

// TestStageReleaseRootFilesFailsWhenAFileIsMissing keeps the staging honest: a
// licence that silently fails to copy is the defect this change exists to fix,
// so its absence must stop the release rather than ship a second time.
func TestStageReleaseRootFilesFailsWhenAFileIsMissing(t *testing.T) {
	repoRoot := t.TempDir()
	payloadDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "LICENSE"), []byte("mit"), 0o644); err != nil {
		t.Fatal(err)
	}
	// SECURITY.md deliberately absent.

	if err := stageReleaseRootFiles(repoRoot, payloadDir); err == nil {
		t.Fatal("stageReleaseRootFiles err = nil, want a failure naming the missing SECURITY.md")
	}
}

// TestPackagedReadmeLinksArePinnedAgainstThePayload is #4268's link pin.
//
// 10 of the packaged README's 25 relative links dangled once extracted —
// LICENSE, CONTRIBUTING.md, go.mod, internal/journal/doc.go and others —
// because the repository's markdown link checker validates against the repo
// tree, where every one of them resolves, and never against the archive layout
// that actually ships.
//
// The rewrite is decided by what the payload contains, so this test builds a
// payload rather than asserting on a list of known-bad paths: the check that
// matters is "does this link work in the thing we shipped".
func TestPackagedReadmeLinksArePinnedAgainstThePayload(t *testing.T) {
	payloadDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(payloadDir, "docs", "guides"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, present := range []string{"LICENSE", filepath.Join("docs", "guides", "quickstart.md")} {
		if err := os.WriteFile(filepath.Join(payloadDir, present), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	readme := strings.Join([]string{
		"# Goobers",
		"[licence](LICENSE)", // in the payload: stays relative
		"[quickstart](docs/guides/quickstart.md)",                         // in the payload: stays relative
		"[quickstart anchor](docs/guides/quickstart.md#install)",          // fragment, still resolves
		"[contributing](CONTRIBUTING.md)",                                 // NOT in the payload: pinned
		"[claim](internal/localscheduler/claim.go)",                       // NOT in the payload: pinned
		"[releases](https://github.com/Agent-Clubhouse/Goobers/releases)", // absolute: untouched
		"[section](#quick-start)",                                         // in-document anchor: untouched
	}, "\n\n")
	if err := os.WriteFile(filepath.Join(payloadDir, "README.md"), []byte(readme), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := pinUnresolvableReadmeLinks(payloadDir, "v0.4.0-rc.1"); err != nil {
		t.Fatalf("pinUnresolvableReadmeLinks: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(payloadDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)

	for _, keep := range []string{"](LICENSE)", "](docs/guides/quickstart.md)", "](docs/guides/quickstart.md#install)"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("README lost %q; a target the archive DOES contain must keep its relative link:\n%s", keep, got)
		}
	}
	// Pinned at the release tag, not main: this README is read months later
	// beside a binary of this vintage, and main will have moved.
	for _, pinned := range []string{
		"](https://github.com/Agent-Clubhouse/Goobers/blob/v0.4.0-rc.1/CONTRIBUTING.md)",
		"](https://github.com/Agent-Clubhouse/Goobers/blob/v0.4.0-rc.1/internal/localscheduler/claim.go)",
	} {
		if !strings.Contains(got, pinned) {
			t.Fatalf("README missing %q; a target the archive does NOT contain must be pinned at the tag:\n%s", pinned, got)
		}
	}
	if !strings.Contains(got, "](https://github.com/Agent-Clubhouse/Goobers/releases)") {
		t.Fatalf("README rewrote an absolute URL:\n%s", got)
	}
	if !strings.Contains(got, "](#quick-start)") {
		t.Fatalf("README rewrote an in-document anchor:\n%s", got)
	}

	broken, err := unresolvableReadmeLinks(payloadDir)
	if err != nil {
		t.Fatalf("unresolvableReadmeLinks: %v", err)
	}
	if len(broken) != 0 {
		t.Fatalf("unresolvable links after pinning = %v, want none", broken)
	}
}

// TestUnresolvableReadmeLinksReportsTheDanglingTargets proves the assertion
// that guards staging actually detects the shipped defect, rather than being
// satisfied by construction: an unpinned README with the real dangling targets
// must be reported.
func TestUnresolvableReadmeLinksReportsTheDanglingTargets(t *testing.T) {
	payloadDir := t.TempDir()
	readme := "[licence](LICENSE)\n\n[contributing](CONTRIBUTING.md)\n\n[mod](go.mod)\n"
	if err := os.WriteFile(filepath.Join(payloadDir, "README.md"), []byte(readme), 0o644); err != nil {
		t.Fatal(err)
	}

	broken, err := unresolvableReadmeLinks(payloadDir)
	if err != nil {
		t.Fatalf("unresolvableReadmeLinks: %v", err)
	}
	want := map[string]bool{"LICENSE": true, "CONTRIBUTING.md": true, "go.mod": true}
	if len(broken) != len(want) {
		t.Fatalf("broken = %v, want the three dangling targets", broken)
	}
	for _, target := range broken {
		if !want[target] {
			t.Fatalf("broken = %v, want exactly %v", broken, want)
		}
	}
}

// TestStagingGuardCatchesADanglingReferenceLink proves the staging guard is not
// satisfied by construction. The pin rewrites inline links only, so a
// reference-style definition is a link form it cannot make true — exactly the
// case the guard exists to stop, and the reason the guard scans a broader set
// of link forms than the pin rewrites.
func TestStagingGuardCatchesADanglingReferenceLink(t *testing.T) {
	payloadDir := t.TempDir()
	readme := "# Goobers\n\nSee the [contributing guide][contrib].\n\n[contrib]: CONTRIBUTING.md\n"
	if err := os.WriteFile(filepath.Join(payloadDir, "README.md"), []byte(readme), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := pinUnresolvableReadmeLinks(payloadDir, "v0.4.0-rc.1"); err != nil {
		t.Fatalf("pinUnresolvableReadmeLinks: %v", err)
	}
	broken, err := unresolvableReadmeLinks(payloadDir)
	if err != nil {
		t.Fatalf("unresolvableReadmeLinks: %v", err)
	}
	if len(broken) != 1 || broken[0] != "CONTRIBUTING.md" {
		t.Fatalf("broken = %v, want [CONTRIBUTING.md]: a dangling reference definition must fail staging, "+
			"not ship", broken)
	}
}
