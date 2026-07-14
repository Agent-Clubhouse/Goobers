package tutor

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// TestDogfoodConfigRootHasCodeowner enforces the governance half of the T4
// (#104) boundary: on the dogfood instance Tutor PRs land in the same repo as
// platform code, so CODEOWNERS must own the config root ("selfhost/") — a
// maintainer review is then required before a self-tuning config change merges.
// This regression-guards the "CODEOWNERS present on the root" enablement
// requirement (see docs/guides/tutor-write-boundary.md).
func TestDogfoodConfigRootHasCodeowner(t *testing.T) {
	raw, err := os.ReadFile("../../.github/CODEOWNERS")
	if err != nil {
		t.Fatalf("read CODEOWNERS: %v", err)
	}
	const root = "/selfhost/"
	owned := false
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if fields[0] == root { // pattern followed by at least one owner
			owned = true
			break
		}
	}
	if !owned {
		t.Fatalf("CODEOWNERS has no owned entry for the dogfood config root %q; "+
			"the Tutor write-boundary requires CODEOWNER review on the config root", root)
	}
}

// TestEnforceConfigBoundaryRejectsPlatformPathsUnderNonDefaultRoot is the T4
// (#104) negative test: with the config root set to a NON-default value
// ("selfhost", the dogfood config root — not a hardcoded "config/"), any path
// outside that root — every platform path — is refused. This proves platform
// paths are unreachable through the Tutor workflow and that the check honors the
// *configured* root rather than a hardcoded one.
func TestEnforceConfigBoundaryRejectsPlatformPathsUnderNonDefaultRoot(t *testing.T) {
	const root = "selfhost" // deliberately not "config/"
	platformPaths := []string{
		"internal/scheduler/scheduler.go",
		".github/workflows/ci.yml",
		".github/CODEOWNERS",
		"Makefile",
		"cmd/goobers/main.go",
		"providers/github.go",
		"config/gaggles/acme/workflows/x.yaml", // the *default* root — must NOT be treated as inside "selfhost"
		"selfhost-evil/secrets.yaml",           // prefix-only match must not pass
		"../platform/secrets.yaml",
		"/etc/passwd",
	}
	for _, p := range platformPaths {
		err := enforceConfigBoundary(root, []providers.CommitFile{{Path: p}})
		if !errors.Is(err, ErrOutsideConfigRoot) {
			t.Errorf("enforceConfigBoundary(%q, %q) = %v; want ErrOutsideConfigRoot", root, p, err)
		}
	}
}

// TestEnforceConfigBoundaryAcceptsPathsInsideNonDefaultRoot: the mirror positive
// case — paths genuinely under the configured (non-default) root are allowed,
// including the exact shapes the Planner emits.
func TestEnforceConfigBoundaryAcceptsPathsInsideNonDefaultRoot(t *testing.T) {
	for _, root := range []string{"selfhost", "selfhost/", "cfg/instance-a", "custom-config"} {
		inside := []string{
			"gaggles/acme/workflows/implement.yaml",
			"gaggles/acme/goobers/coder/instructions.md",
			"manifest.yaml",
		}
		for _, rel := range inside {
			p := normalizeConfigRoot(root) + "/" + rel
			if err := enforceConfigBoundary(root, []providers.CommitFile{{Path: p}}); err != nil {
				t.Errorf("enforceConfigBoundary(%q, %q) = %v; want nil", root, p, err)
			}
		}
	}
}

// TestEnforceConfigBoundaryRejectsEscapesRegardlessOfRoot: absolute paths and
// repo-escaping ".." are refused even in the separate-config-repo (empty root)
// case, where any *in-repo* path is otherwise allowed.
func TestEnforceConfigBoundaryRejectsEscapesRegardlessOfRoot(t *testing.T) {
	cases := []struct {
		root, path string
		wantErr    bool
	}{
		{"", "gaggles/acme/workflows/x.yaml", false}, // separate-config-repo: in-repo path ok
		{"", "manifest.yaml", false},
		{"", "../outside.yaml", true},       // escapes repo
		{"", "/etc/passwd", true},           // absolute
		{"", "", true},                      // empty
		{"selfhost", "", true},              // empty under a root
		{"selfhost", "../x", true},          // escapes repo
		{"selfhost", "selfhost/../x", true}, // ".." that climbs out of the root
	}
	for _, tc := range cases {
		err := enforceConfigBoundary(tc.root, []providers.CommitFile{{Path: tc.path}})
		if tc.wantErr && !errors.Is(err, ErrOutsideConfigRoot) {
			t.Errorf("enforceConfigBoundary(%q, %q) = %v; want ErrOutsideConfigRoot", tc.root, tc.path, err)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("enforceConfigBoundary(%q, %q) = %v; want nil", tc.root, tc.path, err)
		}
	}
}

// TestEnforceConfigBoundaryRejectsAnyEscapingFileInSet: one bad file rejects the
// whole proposal — a mixed set is not partially accepted.
func TestEnforceConfigBoundaryRejectsAnyEscapingFileInSet(t *testing.T) {
	files := []providers.CommitFile{
		{Path: "selfhost/gaggles/acme/workflows/x.yaml"}, // fine
		{Path: "internal/runner/run.go"},                 // escapes
	}
	if err := enforceConfigBoundary("selfhost", files); !errors.Is(err, ErrOutsideConfigRoot) {
		t.Fatalf("mixed set = %v; want ErrOutsideConfigRoot", err)
	}
}

// TestNormalizeConfigRootRefusesBogusRoots: a root that itself escapes collapses
// to "" (whole-repo floor) rather than being trusted as a subtree — a bogus root
// can never widen the boundary.
func TestNormalizeConfigRootRefusesBogusRoots(t *testing.T) {
	for _, bogus := range []string{"", "  ", "/", "//", ".", "..", "../up", "/abs/root"} {
		if got := normalizeConfigRoot(bogus); got != "" {
			t.Errorf("normalizeConfigRoot(%q) = %q; want \"\"", bogus, got)
		}
	}
	if got := normalizeConfigRoot("selfhost/"); got != "selfhost" {
		t.Errorf("normalizeConfigRoot(%q) = %q; want %q", "selfhost/", got, "selfhost")
	}
}
