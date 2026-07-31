package readservice

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
)

// TestBoundedReadRefusesRatherThanScanningWhenDegraded is the central property
// of #1933.
//
// `ListRuns` used to fall through to the journal-scanning path whenever the
// indexed sources were absent. That made "the read path is bounded" a property
// of ONE topology rather than of the service, and gave an operator on the
// standalone dashboard O(total history) per request with nothing to indicate it.
//
// Degraded and building modes now refuse. A caller that gets this can retry,
// show a progress banner, or opt into the scan explicitly — all better than a
// request that appears to work and takes minutes.
func TestBoundedReadRefusesRatherThanScanningWhenDegraded(t *testing.T) {
	for _, mode := range []ReadMode{ReadModeDegraded, ReadModeBuilding} {
		t.Run(string(mode), func(t *testing.T) {
			service := &Local{}
			service.SetReadMode(mode)
			if service.boundedReadAvailable() {
				t.Errorf("mode %s reports bounded reads available; a list would silently "+
					"take the O(total history) scanning path", mode)
			}
		})
	}
}

// TestAuthoritativeIsAvailableButExplicit pins that the scanning path survives
// as a deliberate choice.
//
// It exists for an operator verifying a suspicion against the source of truth,
// and for the differential tests. The requirement is that it is never entered by
// accident.
func TestAuthoritativeIsAvailableButExplicit(t *testing.T) {
	service := &Local{}
	service.SetReadMode(ReadModeAuthoritative)
	if !service.boundedReadAvailable() {
		t.Error("authoritative mode refuses reads; the explicit opt-in must still work")
	}

	// And it is only reached by asking for it.
	store, mode, err := OpenReadModel(TopologyConfig{
		Topology:      TopologyCLI,
		Authoritative: true,
		ReadModelPath: filepath.Join(t.TempDir(), "read.db"),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if mode != ReadModeAuthoritative {
		t.Errorf("mode = %s, want authoritative", mode)
	}
	if store != nil {
		t.Error("authoritative mode opened a read model; the caller asked for the source " +
			"of truth, not a projection that might lag")
	}
}

// TestDefaultModeIsProjected pins that every existing construction is unchanged.
//
// The field is added to a struct with many call sites; a zero value that meant
// "degraded" would silently break every one of them.
func TestDefaultModeIsProjected(t *testing.T) {
	service := &Local{}
	if service.ReadMode() != ReadModeProjected {
		t.Errorf("a zero-valued service reports mode %s, want projected; every existing "+
			"construction would change behaviour", service.ReadMode())
	}
	if !service.boundedReadAvailable() {
		t.Error("a zero-valued service refuses bounded reads")
	}
}

// TestOpenReadModelDegradesRatherThanErroring pins the read-only-volume case.
//
// `goobers dashboard` on a read-only mount is a real situation. Refusing to
// start would be worse than serving single-run routes with a banner — but the
// degradation must be REPORTED, which is the difference from the old silent
// fallback.
func TestOpenReadModelDegradesRatherThanErroring(t *testing.T) {
	// A path inside a file, which cannot be created as a directory entry.
	blocked := filepath.Join(t.TempDir(), "not-a-dir", "nested", "read.db")
	store, mode, err := OpenReadModel(TopologyConfig{
		Topology:      TopologyStandalone,
		ReadModelPath: blocked,
	})
	if err != nil {
		t.Fatalf("an unopenable read model returned an error rather than degrading: %v", err)
	}
	if store != nil {
		t.Error("a store was returned for an unopenable path")
	}
	if mode != ReadModeDegraded {
		t.Errorf("mode = %s, want degraded — and reported, not silent", mode)
	}
}

// TestResolveReadModelPathDefaultsToTheLayout pins that making the path
// configurable does not move it for anyone who has not configured it.
func TestResolveReadModelPathDefaultsToTheLayout(t *testing.T) {
	layout := instance.Layout{Root: t.TempDir()}
	config := TopologyConfig{Topology: TopologyDaemon, Layout: layout}
	if got := config.ResolveReadModelPath(); got != layout.ReadDB() {
		t.Errorf("default path = %q, want the layout's %q", got, layout.ReadDB())
	}

	override := filepath.Join(t.TempDir(), "elsewhere.db")
	config.ReadModelPath = override
	if got := config.ResolveReadModelPath(); got != override {
		t.Errorf("override path = %q, want %q", got, override)
	}
}

// TestEnsureBuiltIsANoOpOnAPopulatedStore pins that a standalone start does not
// rebuild what it already has.
//
// A rebuild on every start would make the dashboard unusable on any instance
// with history — which is exactly the surface this issue exists to fix.
func TestEnsureBuiltIsANoOpOnAPopulatedStore(t *testing.T) {
	ctx := context.Background()
	store, err := readmodel.Open(filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.UpsertRun(ctx, readmodel.Projection{Run: readmodel.RunRow{
		RunID: "run-1", Gaggle: "alpha", Workflow: "wf", LastSeq: 1,
	}}); err != nil {
		t.Fatal(err)
	}

	var progress BuildProgress
	if err := EnsureBuilt(ctx, store, instance.Layout{Root: t.TempDir()}, func(p BuildProgress) {
		progress = p
	}); err != nil {
		t.Fatalf("ensure built: %v", err)
	}
	if progress.Scanned != 0 {
		t.Errorf("a populated store was rebuilt (%d scanned); every standalone start would "+
			"pay for the whole corpus", progress.Scanned)
	}
	if !progress.Done {
		t.Error("progress did not report completion")
	}
}

// TestErrBoundedReadUnavailableIsMatchable pins that callers can branch on it.
func TestErrBoundedReadUnavailableIsMatchable(t *testing.T) {
	wrapped := errors.Join(ErrBoundedReadUnavailable, errors.New("context"))
	if !errors.Is(wrapped, ErrBoundedReadUnavailable) {
		t.Error("the sentinel does not survive wrapping; a caller cannot distinguish it " +
			"from a backend failure and would retry forever")
	}
}

// TestStandaloneResolvesOutsideTheInstance pins the correction to the issue's
// own proposal.
//
// #1933 says standalone should "open read.db; if absent, build it". Taken
// literally that writes into the instance directory — which breaks two things at
// once: `goobers dashboard` is contractually required to leave the instance
// byte-identical (there is a test asserting it), and the read-only volume the
// issue names as the degraded case is exactly where writing fails.
//
// The projection is derived state, reproducible from journals at any time, so a
// cache directory is the correct home. Deleting it costs a rebuild, not data.
func TestStandaloneResolvesOutsideTheInstance(t *testing.T) {
	root := t.TempDir()
	config := TopologyConfig{Topology: TopologyStandalone, Layout: instance.Layout{Root: root}}

	path := config.ResolveReadModelPath()
	if path == "" {
		t.Skip("no user cache directory on this platform")
	}
	if strings.HasPrefix(path, root) {
		t.Errorf("standalone resolved %q inside the instance root %q; the dashboard must "+
			"leave the instance unchanged and must work on a read-only volume", path, root)
	}
}

// TestStandaloneKeysTheCacheByInstance pins that two instances on one machine do
// not share a projection.
//
// Sharing would be silently wrong: each would see the other's runs, and neither
// would be able to tell.
func TestStandaloneKeysTheCacheByInstance(t *testing.T) {
	first := TopologyConfig{Topology: TopologyStandalone, Layout: instance.Layout{Root: t.TempDir()}}
	second := TopologyConfig{Topology: TopologyStandalone, Layout: instance.Layout{Root: t.TempDir()}}

	a, b := first.ResolveReadModelPath(), second.ResolveReadModelPath()
	if a == "" || b == "" {
		t.Skip("no user cache directory on this platform")
	}
	if a == b {
		t.Errorf("two instances resolved the same projection path %q; each would see the "+
			"other's runs with no way to tell", a)
	}
}

// TestDaemonStillUsesTheInstancePath pins that the daemon is unchanged.
//
// It owns its instance directory, and moving its store would orphan every
// existing deployment's projection.
func TestDaemonStillUsesTheInstancePath(t *testing.T) {
	layout := instance.Layout{Root: t.TempDir()}
	config := TopologyConfig{Topology: TopologyDaemon, Layout: layout}
	if got := config.ResolveReadModelPath(); got != layout.ReadDB() {
		t.Errorf("daemon resolved %q, want the instance path %q; moving it would orphan "+
			"every existing deployment's projection", got, layout.ReadDB())
	}
}
