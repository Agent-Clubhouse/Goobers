package readservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
)

// One read topology (#1933, design §11.1–§11.3).
//
// # The problem this removes
//
// The same read library was used in three configurations that are not
// equivalent, and the difference was undocumented. The WORST-performing one is
// what a new user meets first: `goobers dashboard` constructed its read service
// with no Telemetry and no ReadModel at all, so every list was a full scan of
// all history.
//
// Worse, the selection was silent. `ListRuns` fell through to the
// journal-scanning path whenever the indexed sources were absent, so "the read
// path is bounded" was true in exactly one topology and nothing said so. An
// operator on the standalone dashboard had no way to know they were getting
// O(total history) per request.
//
// # The contract
//
// There is no configuration in which the read service has no projection. Either
// a read model is attached, or the service is EXPLICITLY degraded and says so.
// The journal-scanning path survives only behind a deliberate opt-in.

// Topology names how a read service was constructed.
type Topology string

const (
	// TopologyDaemon is the projector in-process with both stores at their
	// configured paths.
	TopologyDaemon Topology = "daemon"
	// TopologyStandalone is `goobers dashboard` with no daemon: the read model
	// is opened, and built if absent.
	TopologyStandalone Topology = "standalone"
	// TopologyCLI is an in-process one-shot command.
	TopologyCLI Topology = "cli"
)

// ReadMode says which path answers a bounded list.
type ReadMode string

const (
	// ReadModeProjected is the normal path: bounded, indexed, zero journal opens.
	ReadModeProjected ReadMode = "projected"
	// ReadModeBuilding means the read model is being constructed. Single-run
	// routes work; bounded routes are unavailable rather than served by a scan.
	ReadModeBuilding ReadMode = "building"
	// ReadModeDegraded means no read model could be built — a read-only volume,
	// typically. Single-run routes only, and the client is told.
	ReadModeDegraded ReadMode = "degraded"
	// ReadModeAuthoritative is the journal-scanning path, entered ONLY by
	// explicit request.
	//
	// It is O(total history) and documented as such. It exists for an operator
	// verifying a suspicion against the source of truth, and for the
	// differential tests — never as a silent fallback, which is what it was.
	ReadModeAuthoritative ReadMode = "authoritative"
)

// ErrBoundedReadUnavailable is returned when a bounded list is requested while
// the read model is building or degraded.
//
// An error rather than a scan. §11.2: "never a silent O(H) scan" — a caller that
// gets this can retry, show a progress banner, or fall back deliberately, all of
// which are better than a request that appears to work and takes minutes.
var ErrBoundedReadUnavailable = errors.New(
	"readservice: bounded reads are unavailable while the read model is building or degraded")

// TopologyConfig describes how to construct a read service.
type TopologyConfig struct {
	Topology Topology
	Layout   instance.Layout
	// ReadModelPath overrides the default location. Empty uses the layout's.
	//
	// Configurable because the hosted wave needs it (§13.2) and because a local
	// operator with a read-only instance volume may want the projection
	// elsewhere. Defaulting to the layout keeps every existing deployment
	// unchanged.
	ReadModelPath string
	// Authoritative opts into the journal-scanning path.
	Authoritative bool
}

// ResolveReadModelPath returns where the read model lives for a config.
//
// # Standalone builds OUTSIDE the instance
//
// The daemon owns its instance directory and puts read.db there. Standalone does
// not: `goobers dashboard` is contractually required to leave the instance
// byte-identical, and there is a test asserting it.
//
// Writing the projection into the instance would also fail on exactly the
// read-only volume §11.2 names as the degraded case — so the issue's own
// proposal to "open read.db, build it if absent" cannot be taken literally for
// standalone without breaking both properties at once.
//
// A cache directory is the semantically correct home anyway: the projection is
// DERIVED state, reproducible from the journals at any time, and deleting it
// costs a rebuild rather than data. Keying it by instance root keeps two
// instances on one machine from sharing a store.
func (c TopologyConfig) ResolveReadModelPath() string {
	if c.ReadModelPath != "" {
		return c.ReadModelPath
	}
	if c.Topology == TopologyStandalone {
		if cached, ok := standaloneCachePath(c.Layout.Root); ok {
			return cached
		}
		// No usable cache directory: fall through to degraded rather than
		// writing into the instance.
		return ""
	}
	return c.Layout.ReadDB()
}

// standaloneCachePath returns a per-instance projection path under the user
// cache directory.
func standaloneCachePath(instanceRoot string) (string, bool) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", false
	}
	absolute, err := filepath.Abs(instanceRoot)
	if err != nil {
		absolute = instanceRoot
	}
	// Hashed rather than path-derived: an instance root can contain characters
	// that are not portable in a path component, and can be long enough to hit
	// path limits on Windows.
	sum := sha256.Sum256([]byte(absolute))
	dir := filepath.Join(base, "goobers", "readmodel", hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false
	}
	return filepath.Join(dir, readmodel.FileName), true
}

// OpenReadModel opens or creates the read model for a topology.
//
// # Failure is degraded, not fatal
//
// A read-only volume, a corrupt file, or a path that cannot be created all
// produce a nil store and a degraded mode rather than an error the caller must
// handle. That is deliberate: `goobers dashboard` on a read-only mount is a real
// situation, and refusing to start would be worse than serving single-run routes
// with a banner.
//
// The distinction from the OLD behaviour is that the degradation is REPORTED. It
// used to be silent, and looked identical to a healthy service that happened to
// be slow.
func OpenReadModel(config TopologyConfig) (*readmodel.Store, ReadMode, error) {
	path := config.ResolveReadModelPath()
	if path == "" {
		// No writable location for a derived store. Degraded, and reported.
		return nil, ReadModeDegraded, nil
	}
	if config.Authoritative {
		// Explicit opt-in wins: the caller asked for the source of truth and
		// should get it, not a projection that might lag.
		return nil, ReadModeAuthoritative, nil
	}
	store, err := readmodel.Open(path)
	if err != nil {
		return nil, ReadModeDegraded, nil
	}
	return store, ReadModeProjected, nil
}

// BuildProgress reports a standalone build in flight.
type BuildProgress struct {
	Scanned   int
	Projected int
	Done      bool
}

// EnsureBuilt builds the read model when it is empty.
//
// Reports progress rather than blocking silently, because on a standalone start
// this is the first thing a user waits on. It is seconds rather than minutes
// now: §5.1's store split means the rebuild input is 191 MB of run events rather
// than the 2.5 GB that includes spans.
func EnsureBuilt(ctx context.Context, store *readmodel.Store, layout instance.Layout,
	report func(BuildProgress)) error {
	if store == nil {
		return nil
	}
	counts, err := store.CountByPhase(ctx)
	if err != nil {
		return fmt.Errorf("readservice: inspect read model: %w", err)
	}
	for _, n := range counts {
		if n > 0 {
			if report != nil {
				report(BuildProgress{Done: true})
			}
			return nil
		}
	}

	roots, err := layout.RunDirs()
	if err != nil {
		return err
	}
	result, err := store.BuildFromJournals(ctx, roots)
	if err != nil {
		return err
	}
	if report != nil {
		report(BuildProgress{Scanned: result.Scanned, Projected: result.Projected, Done: true})
	}
	return nil
}

// SetReadMode records how this service answers bounded reads.
func (s *Local) SetReadMode(mode ReadMode) { s.readMode = mode }

// ReadMode reports how this service answers bounded reads.
func (s *Local) ReadMode() ReadMode {
	if s.readMode == "" {
		return ReadModeProjected
	}
	return s.readMode
}

// boundedReadAvailable reports whether a bounded list can be served.
//
// The check that used to be absent. `ListRuns` silently fell through to the
// journal-scanning path whenever the indexed sources were nil, which made
// "the read path is bounded" a property of one topology rather than of the
// service.
func (s *Local) boundedReadAvailable() bool {
	switch s.ReadMode() {
	case ReadModeProjected, ReadModeAuthoritative:
		return true
	default:
		return false
	}
}
