package configboundary

import (
	"errors"
	"testing"
)

// TestConfineRejectsPlatformPathsUnderNonDefaultRoot is the ported T4 (#104)
// negative test, now exercised on a []string of changed paths: with the config
// root set to a NON-default value ("reference-workflows", the dogfood config root — not a
// hardcoded "config/"), every platform path is refused. Proves platform paths
// are unreachable and that the check honors the configured root.
func TestConfineRejectsPlatformPathsUnderNonDefaultRoot(t *testing.T) {
	const root = "reference-workflows"
	platform := []string{
		"internal/localscheduler/scheduler.go",
		".github/workflows/ci.yml",
		".github/CODEOWNERS",
		"Makefile",
		"cmd/goobers/main.go",
		"providers/github.go",
		"config/gaggles/acme/workflows/x.yaml",  // the *default* root — not inside "reference-workflows"
		"reference-workflows-evil/secrets.yaml", // prefix-only collision must not pass
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
	for _, root := range []string{"reference-workflows", "reference-workflows/", "cfg/instance-a", "custom-config"} {
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
		{"", `\root-relative`, true},
		{"", `C:\absolute`, true},
		{"", `\\server\share`, true},
		{"", "", true},
		{"reference-workflows", "", true},
		{"reference-workflows", "../x", true},
		{"reference-workflows", "reference-workflows/../x", true},
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
		"reference-workflows/gaggles/acme/workflows/x.yaml", // fine
		"internal/runner/run.go",                            // escapes
	}
	if err := Confine("reference-workflows", changed); !errors.Is(err, ErrOutsideConfigRoot) {
		t.Fatalf("mixed set = %v; want ErrOutsideConfigRoot", err)
	}
}

// TestConfineEmptyChangeSetIsAllowed: a cycle proposing no change has nothing to
// confine.
func TestConfineEmptyChangeSetIsAllowed(t *testing.T) {
	if err := Confine("reference-workflows", nil); err != nil {
		t.Fatalf("Confine(reference-workflows, nil) = %v; want nil", err)
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
	if got := normalizeConfigRoot("reference-workflows/"); got != "reference-workflows" {
		t.Errorf("normalizeConfigRoot(%q) = %q; want %q", "reference-workflows/", got, "reference-workflows")
	}
}

func TestConfineRejectsInvalidNonemptyRoot(t *testing.T) {
	for _, root := range []string{".", "..", "../up", "/abs/root", `\rooted`, `C:\rooted`, `\\server\share`} {
		if err := Confine(root, []string{"internal/runner/run.go"}); !errors.Is(err, ErrOutsideConfigRoot) {
			t.Errorf("Confine(%q) = %v; want ErrOutsideConfigRoot", root, err)
		}
	}
	if err := Confine("/", []string{"manifest.yaml"}); err != nil {
		t.Fatalf("slash-only whole-repo root = %v", err)
	}
}

// TestConfineToAnyAcceptsPathsInAnyRoot: a change under any one of several
// declared docs roots is allowed — files can land in docs/, docs/design/, or a
// root-level doc file, and all pass.
func TestConfineToAnyAcceptsPathsInAnyRoot(t *testing.T) {
	roots := []string{"docs", "docs/design", "README.md", "ARCHITECTURE.md"}
	inside := []string{
		"docs/overview.md",
		"docs/design/architecture.md",
		"README.md",
		"ARCHITECTURE.md",
	}
	if err := ConfineToAny(roots, inside); err != nil {
		t.Fatalf("ConfineToAny(%v, %v) = %v; want nil", roots, inside, err)
	}
}

// TestConfineToAnyRejectsPathOutsideEveryRoot: a code path is refused exactly as
// the single-root Confine refuses an out-of-config-root path — one out-of-roots
// file rejects the whole set.
func TestConfineToAnyRejectsPathOutsideEveryRoot(t *testing.T) {
	roots := []string{"docs", "README.md"}
	for _, p := range []string{
		"internal/runner/run.go",
		".github/workflows/ci.yml",
		"docs-evil/secrets.md", // prefix-only collision must not pass
		"../outside.md",
		"/etc/passwd",
	} {
		if err := ConfineToAny(roots, []string{p}); !errors.Is(err, ErrOutsideConfigRoot) {
			t.Errorf("ConfineToAny(%v, %q) = %v; want ErrOutsideConfigRoot", roots, p, err)
		}
	}
	// A mixed set with one escaping path is rejected as a whole.
	if err := ConfineToAny(roots, []string{"docs/ok.md", "internal/x.go"}); !errors.Is(err, ErrOutsideConfigRoot) {
		t.Errorf("mixed set = %v; want ErrOutsideConfigRoot", err)
	}
}

// TestConfineToAnyEmptyRootsFailsClosed: unlike Confine's empty configRoot
// (whole-repo), no docs roots at all is a misconfiguration and refuses every
// change, and a set of only bogus roots collapses to the same fail-closed state
// rather than silently widening to whole-repo.
func TestConfineToAnyEmptyRootsFailsClosed(t *testing.T) {
	if err := ConfineToAny(nil, []string{"docs/x.md"}); !errors.Is(err, ErrNoDocsRoots) {
		t.Errorf("ConfineToAny(nil, ...) = %v; want ErrNoDocsRoots", err)
	}
	if err := ConfineToAny([]string{"", "  ", "/", ".."}, []string{"docs/x.md"}); !errors.Is(err, ErrNoDocsRoots) {
		t.Errorf("ConfineToAny(all-bogus, ...) = %v; want ErrNoDocsRoots", err)
	}
	// A bogus root alongside a real one does not widen the boundary: the real
	// root still confines, and a path outside it is still refused.
	if err := ConfineToAny([]string{"", "docs"}, []string{"docs/x.md"}); err != nil {
		t.Errorf("ConfineToAny([\"\",\"docs\"], in-root) = %v; want nil", err)
	}
	if err := ConfineToAny([]string{"", "docs"}, []string{"internal/x.go"}); !errors.Is(err, ErrOutsideConfigRoot) {
		t.Errorf("ConfineToAny([\"\",\"docs\"], out-of-root) = %v; want ErrOutsideConfigRoot", err)
	}
}

// TestConfineToAnyEmptyChangeSetIsAllowed: no changes, nothing to confine — even
// with roots declared.
func TestConfineToAnyEmptyChangeSetIsAllowed(t *testing.T) {
	if err := ConfineToAny([]string{"docs"}, nil); err != nil {
		t.Fatalf("ConfineToAny(docs, nil) = %v; want nil", err)
	}
}

// TestConfineExclusiveAcceptsSingleRootChangeSet: a change set entirely within
// one declared root passes and reports that root, regardless of which root it
// is — the Tutor's TUT-A5 per-target-action boundary (#1217).
func TestConfineExclusiveAcceptsSingleRootChangeSet(t *testing.T) {
	roots := []string{"reference-workflows", "skills"}
	cases := []struct {
		changed  []string
		wantRoot string
	}{
		{[]string{"reference-workflows/gaggles/goobers/workflows/tutor.yaml"}, "reference-workflows"},
		{[]string{"skills/new-skill/SKILL.md", "skills/new-skill/references/notes.md"}, "skills"},
	}
	for _, c := range cases {
		got, err := ConfineExclusive(roots, c.changed)
		if err != nil {
			t.Fatalf("ConfineExclusive(%v, %v) = %v; want nil", roots, c.changed, err)
		}
		if got != c.wantRoot {
			t.Errorf("ConfineExclusive(%v, %v) matched root = %q, want %q", roots, c.changed, got, c.wantRoot)
		}
	}
}

// TestConfineExclusiveRejectsCrossRootChangeSet is TUT-A5's core acceptance
// case: "a skill-authoring action cannot also edit workflows" — a change set
// spanning two declared roots is refused even though every individual path is
// within SOME declared root, distinguishing ConfineExclusive from ConfineToAny.
func TestConfineExclusiveRejectsCrossRootChangeSet(t *testing.T) {
	roots := []string{"reference-workflows", "skills"}
	changed := []string{"skills/new-skill/SKILL.md", "reference-workflows/gaggles/goobers/workflows/tutor.yaml"}
	if _, err := ConfineExclusive(roots, changed); !errors.Is(err, ErrCrossRootAction) {
		t.Fatalf("ConfineExclusive(%v, %v) = %v; want ErrCrossRootAction", roots, changed, err)
	}
	// The same two roots individually still pass ConfineToAny — proving the
	// exclusivity is ConfineExclusive's own added constraint, not a side effect
	// of an already-stricter containment check.
	if err := ConfineToAny(roots, changed); err != nil {
		t.Fatalf("ConfineToAny(%v, %v) = %v; want nil (sanity: both paths are legitimately in some root)", roots, changed, err)
	}
}

// TestConfineExclusiveRejectsPathOutsideEveryRoot mirrors ConfineToAny's own
// out-of-root rejection.
func TestConfineExclusiveRejectsPathOutsideEveryRoot(t *testing.T) {
	roots := []string{"reference-workflows", "skills"}
	for _, p := range []string{
		"internal/runner/run.go",
		"skills-evil/secrets.md", // prefix-only collision must not pass
		"../outside.md",
	} {
		if _, err := ConfineExclusive(roots, []string{p}); !errors.Is(err, ErrOutsideConfigRoot) {
			t.Errorf("ConfineExclusive(%v, %q) = %v; want ErrOutsideConfigRoot", roots, p, err)
		}
	}
}

// TestConfineExclusiveEmptyRootsFailsClosed mirrors ConfineToAny's empty/bogus
// roots handling.
func TestConfineExclusiveEmptyRootsFailsClosed(t *testing.T) {
	if _, err := ConfineExclusive(nil, []string{"skills/x.md"}); !errors.Is(err, ErrNoDocsRoots) {
		t.Errorf("ConfineExclusive(nil, ...) = %v; want ErrNoDocsRoots", err)
	}
	if _, err := ConfineExclusive([]string{"", "  ", "/", ".."}, []string{"skills/x.md"}); !errors.Is(err, ErrNoDocsRoots) {
		t.Errorf("ConfineExclusive(all-bogus, ...) = %v; want ErrNoDocsRoots", err)
	}
}

// TestConfineExclusiveEmptyChangeSetIsAllowed: no changes, nothing to confine
// and no root to report.
func TestConfineExclusiveEmptyChangeSetIsAllowed(t *testing.T) {
	got, err := ConfineExclusive([]string{"reference-workflows", "skills"}, nil)
	if err != nil {
		t.Fatalf("ConfineExclusive(roots, nil) = %v; want nil", err)
	}
	if got != "" {
		t.Fatalf("ConfineExclusive(roots, nil) matched root = %q, want empty", got)
	}
}

// TestValidateDocsRoot: the config-load lexical check accepts a real
// repo-relative subtree/file and rejects empty, absolute, whole-repo, and
// escaping roots — each with an ErrInvalidDocsRoot the caller surfaces verbatim.
func TestValidateDocsRoot(t *testing.T) {
	for _, ok := range []string{"docs", "docs/design", "README.md", "path/to/ARCHITECTURE.md", "docs/"} {
		if err := ValidateDocsRoot(ok); err != nil {
			t.Errorf("ValidateDocsRoot(%q) = %v; want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "   ", "/abs/docs", ".", "..", "../up", "docs/../.."} {
		if err := ValidateDocsRoot(bad); !errors.Is(err, ErrInvalidDocsRoot) {
			t.Errorf("ValidateDocsRoot(%q) = %v; want ErrInvalidDocsRoot", bad, err)
		}
	}
}
