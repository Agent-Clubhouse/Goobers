package ephemeraltmp

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/procenv"
	"github.com/goobers/goobers/internal/runnercap"
)

// envValue reads the last binding of name from an env slice, which is the one
// exec honors when a name appears more than once.
func envValue(env []string, name string) (string, bool) {
	value, ok := "", false
	for _, entry := range env {
		k, v, cut := strings.Cut(entry, "=")
		if cut && k == name {
			value, ok = v, true
		}
	}
	return value, ok
}

func envCount(env []string, name string) int {
	n := 0
	for _, entry := range env {
		if k, _, ok := strings.Cut(entry, "="); ok && k == name {
			n++
		}
	}
	return n
}

// tempVar is the variable a stage on this platform reads to find its temp
// directory — the one Apply must repoint.
func tempVar() string {
	if runtime.GOOS == "windows" {
		return "TMP"
	}
	return "TMPDIR"
}

// TestEstablishCarvesAPrivateDirectoryUnderTheRoot pins the shape the whole
// binding rests on: the attempt's temp is a NEW directory under the daemon's
// existing temp root — same medium, same quota, different lifetime.
func TestEstablishCarvesAPrivateDirectoryUnderTheRoot(t *testing.T) {
	root := t.TempDir()
	scope, err := Establish(root)
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	t.Cleanup(func() { _ = scope.Reclaim() })

	if got := filepath.Dir(scope.dir); got != filepath.Clean(root) {
		t.Fatalf("scope parent = %q, want the temp root %q", got, root)
	}
	if !strings.HasPrefix(filepath.Base(scope.dir), dirPrefix) {
		t.Fatalf("scope directory %q does not carry the %q prefix that identifies it on a host", scope.dir, dirPrefix)
	}
	info, err := os.Stat(scope.dir)
	if err != nil {
		t.Fatalf("stat scope dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("scope path %q is not a directory", scope.dir)
	}
	if scope.root != filepath.Clean(root) {
		t.Fatalf("Root() = %q, want %q", scope.root, root)
	}
}

// TestEstablishDefaultsToTheDaemonTempRoot: an empty root means os.TempDir(),
// so the ephemeral area lands on the same medium the stage's temp would
// otherwise have used. Binding the effect changes WHEN those bytes die, never
// WHERE they live.
func TestEstablishDefaultsToTheDaemonTempRoot(t *testing.T) {
	outer := t.TempDir()
	t.Setenv("TMPDIR", outer)
	if os.TempDir() != outer {
		t.Skipf("os.TempDir() = %q, not the TMPDIR this test set (%q); platform does not honor TMPDIR", os.TempDir(), outer)
	}
	scope, err := Establish("")
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	t.Cleanup(func() { _ = scope.Reclaim() })
	if got := filepath.Dir(scope.dir); got != filepath.Clean(outer) {
		t.Fatalf("scope parent = %q, want os.TempDir() %q", got, outer)
	}
}

// TestEstablishFailsClosedOnAnUnusableRoot: the caller has been told the
// runner ENFORCES tmp:ephemeral, so an unusable root must surface as an error
// it can refuse the stage with — never a silent fall back to ambient temp.
func TestEstablishFailsClosedOnAnUnusableRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the file-as-a-directory-parent case is a POSIX ENOTDIR shape")
	}
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope, err := Establish(filepath.Join(blocker, "under-a-file"))
	if err == nil {
		_ = scope.Reclaim()
		t.Fatal("Establish under a regular file returned no error; the binding must fail closed")
	}
	if scope != nil {
		t.Fatalf("Establish returned a scope alongside its error: %+v", scope)
	}
	if !strings.Contains(err.Error(), "ephemeraltmp") {
		t.Fatalf("error %q does not name the package, so a stage refusal would not say what failed", err)
	}
}

// TestApplyRepointsTempAndRerootsTempNestedCaches is the #3949 case exactly:
// GOCACHE configured inside the temp root follows temp into the attempt, which
// is byte-for-byte what a dispatched pod's fresh emptyDir already gives the
// same stage.
func TestApplyRepointsTempAndRerootsTempNestedCaches(t *testing.T) {
	root := t.TempDir()
	scope, err := Establish(root)
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	t.Cleanup(func() { _ = scope.Reclaim() })

	env := []string{
		tempVar() + "=" + root,
		"GOCACHE=" + filepath.Join(root, "gocache"),
		"GOMODCACHE=" + filepath.Join(root, "gomodcache"),
		"PATH=/usr/bin:/bin",
	}
	got, err := scope.Apply(env)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if value, ok := envValue(got, tempVar()); !ok || value != scope.dir {
		t.Fatalf("%s = %q (present=%v), want the attempt-private directory %q", tempVar(), value, ok, scope.dir)
	}
	if n := envCount(got, tempVar()); n != 1 {
		t.Fatalf("%s appears %d times; the scope must be named exactly once", tempVar(), n)
	}
	for name, want := range map[string]string{
		"GOCACHE":    filepath.Join(scope.dir, "gocache"),
		"GOMODCACHE": filepath.Join(scope.dir, "gomodcache"),
	} {
		value, ok := envValue(got, name)
		if !ok {
			t.Fatalf("%s is absent from the applied environment", name)
		}
		if value != want {
			t.Fatalf("%s = %q, want %q", name, value, want)
		}
		if info, err := os.Stat(value); err != nil || !info.IsDir() {
			t.Fatalf("%s directory %q was not created (stat err %v)", name, value, err)
		}
		if !strings.HasPrefix(value, scope.dir+string(filepath.Separator)) {
			t.Fatalf("%s = %q is not inside the attempt-private directory %q", name, value, scope.dir)
		}
	}
	if value, _ := envValue(got, "PATH"); value != "/usr/bin:/bin" {
		t.Fatalf("PATH = %q, want it untouched", value)
	}
	if len(env) != 4 || !strings.HasSuffix(env[1], filepath.Join(root, "gocache")) {
		t.Fatalf("Apply mutated its input slice: %v", env)
	}
}

// TestApplyLeavesCachesOutsideTheTempRootAlone is the behavior-preservation
// guarantee: an install whose caches live in HOME sees no change at all, which
// is why binding this effect cannot cost anyone their warm cache by surprise.
func TestApplyLeavesCachesOutsideTheTempRootAlone(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	scope, err := Establish(root)
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	t.Cleanup(func() { _ = scope.Reclaim() })

	outside := filepath.Join(elsewhere, "go-build")
	got, err := scope.Apply([]string{"GOCACHE=" + outside})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if value, _ := envValue(got, "GOCACHE"); value != outside {
		t.Fatalf("GOCACHE = %q, want the out-of-temp value %q untouched", value, outside)
	}
}

// TestApplyLeavesNonCacheVariablesAlone: install roots and toolchain homes are
// deliberately NOT members of RelocatedVars. Re-rooting an installed SDK
// points the stage at an empty directory — breakage wearing isolation's
// clothes.
func TestApplyLeavesNonCacheVariablesAlone(t *testing.T) {
	root := t.TempDir()
	scope, err := Establish(root)
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	t.Cleanup(func() { _ = scope.Reclaim() })

	installed := filepath.Join(root, "sdk")
	for _, name := range []string{"JAVA_HOME", "DOTNET_ROOT", "RUSTUP_HOME", "GOPATH", "VIRTUAL_ENV", "PLAYWRIGHT_BROWSERS_PATH"} {
		got, err := scope.Apply([]string{name + "=" + installed})
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if value, _ := envValue(got, name); value != installed {
			t.Fatalf("%s = %q, want the installed root %q left alone", name, value, installed)
		}
	}
}

// TestApplySetsTheTempVariableEvenWhenAbsent: a daemon with no TMPDIR set
// still has to hand its stage a private temp, or the stage falls through to
// the platform default — the shared /tmp this binding exists to get out of.
func TestApplySetsTheTempVariableEvenWhenAbsent(t *testing.T) {
	root := t.TempDir()
	scope, err := Establish(root)
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	t.Cleanup(func() { _ = scope.Reclaim() })

	got, err := scope.Apply([]string{"PATH=/bin"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, name := range TempVars() {
		if value, ok := envValue(got, name); !ok || value != scope.dir {
			t.Fatalf("%s = %q (present=%v), want %q", name, value, ok, scope.dir)
		}
	}
}

// TestApplyDropsEveryPlatformTempSpellingFromTheRoot: a unix daemon carrying a
// stray TMP=<root> must not hand the stage a second, shared temp location
// beside the private one it was just given.
func TestApplyDropsEveryPlatformTempSpellingFromTheRoot(t *testing.T) {
	root := t.TempDir()
	scope, err := Establish(root)
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	t.Cleanup(func() { _ = scope.Reclaim() })

	got, err := scope.Apply([]string{"TMPDIR=" + root, "TMP=" + root, "TEMP=" + root})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, entry := range got {
		name, value, _ := strings.Cut(entry, "=")
		if value == filepath.Clean(root) {
			t.Fatalf("%s still names the shared temp root %q after Apply", name, root)
		}
	}
}

// TestApplyPassesThroughMalformedEntries: an env entry with no '=' is not this
// package's to interpret, and dropping it would silently change a stage's
// environment.
func TestApplyPassesThroughMalformedEntries(t *testing.T) {
	root := t.TempDir()
	scope, err := Establish(root)
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	t.Cleanup(func() { _ = scope.Reclaim() })

	got, err := scope.Apply([]string{"MALFORMED"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Contains(got, "MALFORMED") {
		t.Fatalf("applied env %v dropped the malformed entry", got)
	}
}

// TestReclaimDestroysTheAttemptTemp is the reclaim half of the effect: bytes
// written during the attempt are gone when the attempt returns.
func TestReclaimDestroysTheAttemptTemp(t *testing.T) {
	root := t.TempDir()
	scope, err := Establish(root)
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	cache := filepath.Join(scope.dir, "gocache", "ab")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "entry"), []byte("build output"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := scope.Reclaim(); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if _, err := os.Stat(scope.dir); !os.IsNotExist(err) {
		t.Fatalf("attempt temp survived Reclaim (stat err %v)", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("Reclaim damaged the temp root it was carved from: %v", err)
	}
}

// TestReclaimDoesNotTouchAnotherAttemptsTemp is the no-cross-run-deletion
// invariant. The API pod runs stages concurrently; a reclaim that reached past
// its own directory would delete a live run's build cache mid-compile.
func TestReclaimDoesNotTouchAnotherAttemptsTemp(t *testing.T) {
	root := t.TempDir()
	first, err := Establish(root)
	if err != nil {
		t.Fatalf("Establish first: %v", err)
	}
	second, err := Establish(root)
	if err != nil {
		t.Fatalf("Establish second: %v", err)
	}
	t.Cleanup(func() { _ = second.Reclaim() })

	if first.dir == second.dir {
		t.Fatalf("two concurrent attempts share the directory %q", first.dir)
	}
	survivor := filepath.Join(second.dir, "gocache", "entry")
	if err := os.MkdirAll(filepath.Dir(survivor), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(survivor, []byte("still building"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A sibling of the scopes that is not a scope at all: neither reclaim may
	// reach it either.
	bystander := filepath.Join(root, "unrelated")
	if err := os.WriteFile(bystander, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := first.Reclaim(); err != nil {
		t.Fatalf("Reclaim first: %v", err)
	}
	if got, err := os.ReadFile(survivor); err != nil || string(got) != "still building" {
		t.Fatalf("the concurrent attempt's temp was damaged: content %q, err %v", got, err)
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Fatalf("Reclaim removed an unrelated sibling of the scope: %v", err)
	}
	if _, err := os.Stat(second.dir); err != nil {
		t.Fatalf("the concurrent attempt's directory was removed: %v", err)
	}
}

// TestReclaimIsIdempotent: callers defer Reclaim immediately after Establish,
// so it runs on paths where the attempt already reclaimed, and on paths where
// the attempt never started.
func TestReclaimIsIdempotent(t *testing.T) {
	root := t.TempDir()
	scope, err := Establish(root)
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	if err := scope.Reclaim(); err != nil {
		t.Fatalf("first Reclaim: %v", err)
	}
	if err := scope.Reclaim(); err != nil {
		t.Fatalf("second Reclaim: %v", err)
	}
	var nilScope *Scope
	if err := nilScope.Reclaim(); err != nil {
		t.Fatalf("nil Reclaim: %v", err)
	}
	if got, err := nilScope.Apply([]string{"PATH=/bin"}); err != nil || len(got) != 1 {
		t.Fatalf("nil Apply = %v, %v; want the env unchanged so callers need no branch", got, err)
	}
}

// TestReclaimRefusesAPathItDidNotCreate. Reclaim runs on every stage attempt
// of a long-lived daemon and calls RemoveAll on a path assembled from operator
// configuration; deleting the temp root itself must be structurally
// impossible, not merely unlikely.
func TestReclaimRefusesAPathItDidNotCreate(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(root, "keep")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, scope := range map[string]*Scope{
		"the temp root itself":      {root: root, dir: root},
		"a sibling we did not make": {root: root, dir: filepath.Join(root, "someone-elses")},
		"a path outside the root":   {root: root, dir: filepath.Join(t.TempDir(), dirPrefix+"elsewhere")},
	} {
		if err := os.MkdirAll(scope.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		err := scope.Reclaim()
		if err == nil {
			t.Fatalf("%s: Reclaim returned no error; it must refuse a path this package did not create", name)
		}
		if _, statErr := os.Stat(scope.dir); statErr != nil {
			t.Fatalf("%s: Reclaim deleted %q despite refusing it: %v", name, scope.dir, statErr)
		}
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("a refused Reclaim still damaged the temp root: %v", err)
	}
}

// TestRelocatedVarsAreCarriedByProcenv pins the relocation set against the
// allowlist that decides what reaches a stage at all. A member that drifts out
// of procenv.Vars is dead code pretending to be enforcement — it would never
// appear in a stage environment, so re-rooting it could never happen.
func TestRelocatedVarsAreCarriedByProcenv(t *testing.T) {
	for _, name := range RelocatedVars {
		if !slices.Contains(procenv.Vars, name) {
			t.Errorf("RelocatedVars carries %q, which procenv.Vars does not: it can never reach a stage subprocess", name)
		}
	}
	for _, name := range TempVars() {
		if !slices.Contains(procenv.Vars, name) {
			t.Errorf("TempVars carries %q, which procenv.Vars does not", name)
		}
	}
	if got := append([]string(nil), RelocatedVars...); !slices.IsSorted(got) {
		t.Fatalf("RelocatedVars is not sorted, which makes the closed list hard to review: %v", got)
	}
}

// TestRelocatedVarsExcludeInstallRoots states the membership rule as an
// assertion rather than only as prose: a variable naming something a stage
// cannot rebuild on demand must never be re-rooted.
func TestRelocatedVarsExcludeInstallRoots(t *testing.T) {
	for _, name := range []string{
		"DOTNET_ROOT", "JAVA_HOME", "JDK_HOME", "M2_HOME", "RUSTUP_HOME",
		"VIRTUAL_ENV", "GOPATH", "GOBIN", "PLAYWRIGHT_BROWSERS_PATH", "PATH", "HOME",
	} {
		if slices.Contains(RelocatedVars, name) {
			t.Errorf("RelocatedVars must not carry %q: re-rooting it points the stage at an empty directory", name)
		}
	}
}

// TestBindsTheClosedListEffect ties this package to the vocabulary entry it
// implements, so a rename of the effect cannot leave the binding orphaned.
func TestBindsTheClosedListEffect(t *testing.T) {
	if !runnercap.KnownRestriction(string(runnercap.RestrictionTmpEphemeral)) {
		t.Fatal("tmp:ephemeral is not a member of the closed v1 restriction list")
	}
	if !runnercap.DeclarableOnWindows(runnercap.RestrictionTmpEphemeral) {
		t.Fatal("tmp:ephemeral must stay declarable on Windows; this package binds it on every self host")
	}
}
