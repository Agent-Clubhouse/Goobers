// Package ephemeraltmp is the modes-1/2 binding of the `tmp:ephemeral`
// restriction (runnercap.RestrictionTmpEphemeral, the closed v1 effect list in
// docs/design/goobernetes-restrictions.md §2.4): temp space is
// stage-attempt-private and destroyed with the attempt, so nothing written to
// it survives to, or is visible from, any other attempt or stage.
//
// Mode 3 gets the effect from the substrate — a dispatched stage pod is fresh
// and never reused, and internal/dispatcher/podspec.go stampVolumes mounts a
// sized emptyDir at the platform temp path with TMPDIR/TMP/TEMP pointed at it.
// Runner `self` has no pod to be fresh: the daemon process is long-lived and
// every locally executed stage shares the daemon's own temp root. The design
// named the missing half ("Modes 1/2: per-stage TMPDIR ...; deletion rides
// stage cleanup") and the §3 matrix already claimed it; this package is that
// binding, and it is deliberately a leaf — stdlib only — so both self
// execution seams (internal/executor's deterministic shell stage and
// internal/harness's agentic adapters) can bind the same effect the same way
// rather than growing two copies that drift.
//
// Why this matters beyond tidiness: on the prod AKS instance the API pod is
// simultaneously the control plane and the CI executor, and the Go build cache
// its on-pod stages write (`GOCACHE=/tmp/gocache`) grew past 10 GB with no
// reclaim, filling the pod's memory cgroup with page cache until the OOM
// killer took pid 1.
package ephemeraltmp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// dirPrefix names the per-attempt directories this package creates. It is
// grep-able on a host being inspected mid-run, and it is what tells a
// leftover from a crashed daemon apart from anything else in the temp root.
const dirPrefix = "goobers-ephemeral-tmp-"

// TempVars are the environment variables that NAME the temp directory itself,
// per platform — the same pair the dispatcher's Windows binding sets
// (podspec.go stampVolumes: TMPDIR on Linux, TMP and TEMP on Windows), so the
// self binding and the pod binding present an identical contract to a stage
// that reads its own temp location.
func TempVars() []string {
	if runtime.GOOS == "windows" {
		return []string{"TMP", "TEMP"}
	}
	return []string{"TMPDIR"}
}

// RelocatedVars is the closed set of BUILD AND PACKAGE CACHE variables that
// follow temp into the attempt when — and only when — their configured value
// already lives inside the temp root (see Scope.Apply). Every member is a
// directory its toolchain CREATES ON DEMAND and can rebuild from the network
// or from source, which is the whole membership rule: destroying it with the
// attempt costs time, never correctness.
//
// What is deliberately absent is as load-bearing as what is present. Install
// roots and toolchain homes — DOTNET_ROOT, JAVA_HOME, JDK_HOME, M2_HOME,
// RUSTUP_HOME, VIRTUAL_ENV, GOPATH/GOBIN, PLAYWRIGHT_BROWSERS_PATH — are NOT
// members even though procenv carries them: re-rooting an installed toolchain
// points a stage at an empty directory, which is not isolation but breakage.
// A stage that stores its SDK in temp is already broken under any binding of
// this effect; this package refuses to be the thing that breaks it.
//
// TestRelocatedVarsAreCarriedByProcenv pins every member against
// procenv.Vars: a variable procenv does not carry can never reach a stage
// subprocess, so a member that drifts out of that allowlist is dead code
// pretending to be enforcement.
var RelocatedVars = []string{
	"CARGO_HOME",
	"GOCACHE",
	"GOMODCACHE",
	"GRADLE_USER_HOME",
	"NUGET_HTTP_CACHE_PATH",
	"NUGET_PACKAGES",
	"PIP_CACHE_DIR",
	"npm_config_cache",
}

// relocated is RelocatedVars as a set, plus the platform temp variables. The
// temp variables are relocatable for the same reason the caches are: a stage
// env carrying TMPDIR=<temp root> must come out of Apply naming the scope, and
// letting the containment rule do that uniformly is one rule instead of two.
// Every platform's temp names are included regardless of GOOS — a Windows TMP
// value in a unix daemon's environment is still a path into temp, and leaving
// it pointed at the shared root would be a hole with no upside.
var relocated = func() map[string]struct{} {
	set := make(map[string]struct{}, len(RelocatedVars)+3)
	for _, name := range RelocatedVars {
		set[name] = struct{}{}
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		set[name] = struct{}{}
	}
	return set
}()

// Scope is one attempt-private temp area: a directory carved out of root, the
// env rewrites that point a stage at it, and the reclaim that destroys it.
//
// A Scope belongs to exactly one stage attempt. Uniqueness comes from
// os.MkdirTemp, so two concurrent attempts — of the same stage, of the same
// run, of different runs — never share a directory and Reclaim can never
// reach another attempt's bytes.
type Scope struct {
	root string
	dir  string
}

// Establish creates the attempt-private directory under root and returns the
// Scope that owns it. An empty root means the daemon's own temp root
// (os.TempDir(), which honors TMPDIR), which is the point: the ephemeral area
// sits on the SAME medium and under the same quota as the temp the stage would
// otherwise have used unbounded, so binding this effect changes the lifetime
// of those bytes without changing where they live. Sizing that medium stays an
// operator/deployment concern.
//
// It returns an error rather than degrading when the directory cannot be
// created. A caller that has been told to enforce this effect must fail the
// attempt closed (restrictions §4 idiom (b)): running a stage with ambient
// temp under a declared restriction is exactly the confident-PASS-on-
// unenforced-substrate failure the closed list exists to prevent.
func Establish(root string) (*Scope, error) {
	if root == "" {
		root = os.TempDir()
	}
	root = filepath.Clean(root)
	if root == "" || root == "." {
		return nil, errors.New("ephemeraltmp: temp root resolved to an empty path")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("ephemeraltmp: create temp root %q: %w", root, err)
	}
	dir, err := os.MkdirTemp(root, dirPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("ephemeraltmp: create attempt-private temp directory under %q: %w", root, err)
	}
	return &Scope{root: root, dir: dir}, nil
}

// Apply returns a COPY of env with the attempt's temp binding applied:
//
//   - every relocatable variable whose value is a path inside the temp root is
//     re-rooted to the same relative position inside the scope, so
//     GOCACHE=/tmp/gocache becomes <scope>/gocache — byte-for-byte what a
//     dispatched pod's fresh emptyDir already gives that same stage;
//   - every relocatable variable pointing OUTSIDE the temp root is passed
//     through untouched, because it is not temp space and this effect makes no
//     claim about it. That is what keeps the binding behavior-preserving for
//     every install whose caches live in HOME rather than in temp;
//   - the platform temp variables (TempVars) are then set to the scope,
//     whether or not the incoming env named them.
//
// Every re-rooted target directory is created eagerly. A toolchain generally
// creates its own cache directory, but "generally" is not a contract worth
// betting a stage failure on, and an empty directory costs nothing.
//
// env is not modified. A nil Scope returns env unchanged, so a caller that
// did not bind the effect needs no branch.
func (s *Scope) Apply(env []string) ([]string, error) {
	if s == nil {
		return env, nil
	}
	names := TempVars()
	out := make([]string, 0, len(env)+len(names))
	tempVar := make(map[string]struct{}, len(names))
	for _, name := range names {
		tempVar[name] = struct{}{}
	}
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			out = append(out, entry)
			continue
		}
		if _, isTemp := tempVar[name]; isTemp {
			// Written unconditionally below; drop the incoming binding so the
			// result names the scope exactly once.
			continue
		}
		if _, relocatable := relocated[name]; !relocatable {
			out = append(out, entry)
			continue
		}
		target, inside := s.rebase(value)
		if !inside {
			out = append(out, entry)
			continue
		}
		if err := os.MkdirAll(target, 0o700); err != nil {
			return nil, fmt.Errorf("ephemeraltmp: create attempt-private %s directory: %w", name, err)
		}
		out = append(out, name+"="+target)
	}
	for _, name := range names {
		out = append(out, name+"="+s.dir)
	}
	return out, nil
}

// rebase maps a path inside the temp root onto the same relative position
// inside the scope. It reports false — leave the value alone — for a relative
// path, a path outside the root, and for the scope's own directory (already
// attempt-private; re-rooting it a second time would nest it).
func (s *Scope) rebase(value string) (string, bool) {
	if value == "" || !filepath.IsAbs(value) {
		return "", false
	}
	clean := filepath.Clean(value)
	if clean == s.dir || strings.HasPrefix(clean, s.dir+string(filepath.Separator)) {
		return "", false
	}
	if clean == s.root {
		return s.dir, true
	}
	rel, err := filepath.Rel(s.root, clean)
	if err != nil {
		return "", false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Join(s.dir, rel), true
}

// Reclaim destroys the attempt's temp area and nothing else.
//
// The guard is not ceremony. This function runs on every stage attempt on a
// long-lived daemon host, holds a path assembled from operator configuration,
// and calls os.RemoveAll; the one failure that must be impossible is deleting
// the temp ROOT, a sibling attempt's directory, or anything a caller passed in
// by mistake. So it removes only a path this package itself created under the
// root it recorded, and refuses — loudly, without deleting — anything else.
//
// Reclaim is idempotent and safe on a nil Scope, so callers can defer it
// immediately after Establish without tracking whether the attempt got as far
// as running.
func (s *Scope) Reclaim() error {
	if s == nil || s.dir == "" {
		return nil
	}
	dir := s.dir
	parent := filepath.Dir(dir)
	if parent != s.root || !strings.HasPrefix(filepath.Base(dir), dirPrefix) {
		return fmt.Errorf("ephemeraltmp: refusing to reclaim %q: not a directory this package created under %q", dir, s.root)
	}
	s.dir = ""
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("ephemeraltmp: reclaim %q: %w", dir, err)
	}
	return nil
}
