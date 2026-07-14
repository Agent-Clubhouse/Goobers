package configboundary

import (
	"errors"
	"testing"
)

// TestConfineRejectsPlatformPathsUnderNonDefaultRoot is the ported T4 (#104)
// negative test, now exercised on a []string of changed paths: with the config
// root set to a NON-default value ("selfhost", the dogfood config root — not a
// hardcoded "config/"), every platform path is refused. Proves platform paths
// are unreachable and that the check honors the configured root.
func TestConfineRejectsPlatformPathsUnderNonDefaultRoot(t *testing.T) {
	const root = "selfhost"
	platform := []string{
		"internal/scheduler/scheduler.go",
		".github/workflows/ci.yml",
		".github/CODEOWNERS",
		"Makefile",
		"cmd/goobers/main.go",
		"providers/github.go",
		"config/gaggles/acme/workflows/x.yaml", // the *default* root — not inside "selfhost"
		"selfhost-evil/secrets.yaml",           // prefix-only collision must not pass
		"../platform/secrets.yaml",
		"/etc/passwd",
	}
	for _, p := range platform {
		if err := Confine(root, []string{p}); !errors.Is(err, ErrOutsideConfigRoot) {
			t.Errorf("Confine(%q, %q) = %v; want ErrOutsideConfigRoot", root, p, err)
		}
	}
}

// TestConfineAcceptsPathsInsideNonDefaultRoot: paths genuinely under the
// configured (non-default) root are allowed, including the shapes a Tutor config
// change produces.
func TestConfineAcceptsPathsInsideNonDefaultRoot(t *testing.T) {
	for _, root := range []string{"selfhost", "selfhost/", "cfg/instance-a", "custom-config"} {
		norm := normalizeConfigRoot(root)
		inside := []string{
			norm + "/gaggles/acme/workflows/implement.yaml",
			norm + "/gaggles/acme/goobers/coder/instructions.md",
			norm + "/manifest.yaml",
		}
		if err := Confine(root, inside); err != nil {
			t.Errorf("Confine(%q, %v) = %v; want nil", root, inside, err)
		}
	}
}

// TestConfineRejectsEscapesRegardlessOfRoot: absolute paths and repo-escaping
// ".." are refused even in the separate-config-repo (empty root) case.
func TestConfineRejectsEscapesRegardlessOfRoot(t *testing.T) {
	cases := []struct {
		root, path string
		wantErr    bool
	}{
		{"", "gaggles/acme/workflows/x.yaml", false}, // separate-config-repo: in-repo ok
		{"", "manifest.yaml", false},
		{"", "../outside.yaml", true},
		{"", "/etc/passwd", true},
		{"", "", true},
		{"selfhost", "", true},
		{"selfhost", "../x", true},
		{"selfhost", "selfhost/../x", true},
	}
	for _, tc := range cases {
		err := Confine(tc.root, []string{tc.path})
		if tc.wantErr && !errors.Is(err, ErrOutsideConfigRoot) {
			t.Errorf("Confine(%q, %q) = %v; want ErrOutsideConfigRoot", tc.root, tc.path, err)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("Confine(%q, %q) = %v; want nil", tc.root, tc.path, err)
		}
	}
}

// TestConfineRejectsAnyEscapingPathInSet: one bad path rejects the whole change
// set — a mixed diff is not partially accepted.
func TestConfineRejectsAnyEscapingPathInSet(t *testing.T) {
	changed := []string{
		"selfhost/gaggles/acme/workflows/x.yaml", // fine
		"internal/runner/run.go",                 // escapes
	}
	if err := Confine("selfhost", changed); !errors.Is(err, ErrOutsideConfigRoot) {
		t.Fatalf("mixed set = %v; want ErrOutsideConfigRoot", err)
	}
}

// TestConfineEmptyChangeSetIsAllowed: a cycle proposing no change has nothing to
// confine.
func TestConfineEmptyChangeSetIsAllowed(t *testing.T) {
	if err := Confine("selfhost", nil); err != nil {
		t.Fatalf("Confine(selfhost, nil) = %v; want nil", err)
	}
}

// TestNormalizeConfigRootRefusesBogusRoots: a bogus/escaping root collapses to
// "" (whole-repo floor) rather than being trusted as a subtree.
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
