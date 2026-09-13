// Package readservice projects provisioned definitions, journals, and
// telemetry into the versioned runtime read contract shared by HTTP and CLI
// adapters.
package readservice

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/fleet"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/selfupdate"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/version"
)

// defaultFleetEnrolled checks the real Fleet file storage. Errors other than
// "not associated" are treated as not-enrolled — a read-only status field
// must never fail the whole Instance() response over a Fleet storage hiccup.
func defaultFleetEnrolled(instanceRoot string) bool {
	storage, err := fleet.NewFileStorage("")
	if err != nil {
		return false
	}
	_, err = storage.LoadAssociation(instanceRoot)
	return err == nil
}

const (
	// APIVersion identifies the HTTP route version exposing this contract.
	APIVersion = "v1"
	// SchemaVersion identifies the health response schema.
	SchemaVersion = "v1"
)

// Reader is the shared read boundary used by transport and presentation
// adapters. Later read-model slices extend this interface rather than reading
// journals, definitions, or SQLite from their handlers.
type Reader interface {
	Health(context.Context) (Health, error)
	PortalConfig(context.Context) (PortalConfig, error)
	TelemetryReader
	ListRuns(context.Context, RunListOptions) (RunList, error)
	GetRun(context.Context, string) (RunDetail, error)
	RunEvents(context.Context, string) (EventList, error)
	StageAttempts(context.Context, string, string) (AttemptList, error)
	Artifact(context.Context, string, string) (ArtifactContent, error)
	Transcript(context.Context, string, uint64) (TranscriptContent, error)
	Instance(context.Context) (Instance, error)
	Gaggles(context.Context, PageRequest) (GagglePage, error)
	Goobers(context.Context, string, PageRequest) (GooberPage, error)
	Workflows(context.Context, string, PageRequest) (WorkflowPage, error)
	Connections(context.Context, string) (GaggleConnections, error)
	Workflow(context.Context, string, string) (WorkflowDetail, error)
	QueueEligibility(context.Context, string, string) (QueueEligibilityView, error)
}

// Health is the versioned daemon health response.
type Health struct {
	ReadStateEnvelope
	APIVersion       string                  `json:"apiVersion"`
	SchemaVersion    string                  `json:"schemaVersion"`
	Build            BuildMetadata           `json:"build"`
	Ready            bool                    `json:"ready"`
	Healthy          bool                    `json:"healthy"`
	Instance         InstanceIdentity        `json:"instance"`
	Freshness        Freshness               `json:"freshness"`
	DefinitionReload *DefinitionReloadStatus `json:"definitionReload,omitempty"`
	Startup          *StartupStatus          `json:"startup,omitempty"`
	// Update reports whether a newer release exists, so the portal can surface
	// what #4903 gave only terminal users. Nil means no check has run yet (a
	// daemon that just started, or one with updateCheck.enabled: false) —
	// absent, deliberately, rather than a zero value that would read as
	// "confirmed up to date".
	Update *UpdateAvailability `json:"update,omitempty"`
}

// UpdateAvailability is the daemon's last notify-only release check, read from
// its on-disk cache. The daemon performs the check on its own interval; this
// read NEVER contacts the release source, so serving /api/v1/health stays a
// local read and the browser never talks to GitHub.
type UpdateAvailability struct {
	// Available reports LatestVersion > the running build by SemVer.
	Available bool `json:"available"`
	// LatestVersion is the newest release tag on the configured channel.
	LatestVersion string `json:"latestVersion"`
	// CurrentVersion is the build the verdict was computed against. It is
	// always the running build — a cache written by a different binary is
	// refused rather than served — and is carried so a consumer can show what
	// the available version is being compared with.
	CurrentVersion string `json:"currentVersion"`
	// Channel is the channel the check resolved through (stable or prerelease).
	Channel string `json:"channel"`
	// CheckedAt is when the daemon last completed a check, so a consumer can
	// tell a fresh answer from one left by a daemon that has since lost
	// network access.
	CheckedAt time.Time `json:"checkedAt"`
}

// BuildMetadata identifies the exact daemon binary serving the response.
type BuildMetadata struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// InstanceIdentity is the canonical identity provisioned by the manifest.
type InstanceIdentity struct {
	Name        string            `json:"name"`
	Environment apiv1.Environment `json:"environment"`
}

// Freshness describes when the service observed its read sources.
type Freshness struct {
	ObservedAt          time.Time  `json:"observedAt"`
	DefinitionsLoadedAt time.Time  `json:"definitionsLoadedAt"`
	JournalUpdatedAt    *time.Time `json:"journalUpdatedAt"`
	LastSchedulerTickAt *time.Time `json:"lastSchedulerTickAt"`
	LastTickAgeMillis   *int64     `json:"lastTickAgeMillis"`
}

type cachedUpdateCheck struct {
	modTime time.Time
	size    int64
	result  selfupdate.CheckResult
	valid   bool
}

// LocalSources are the three local projections behind the shared service.
type LocalSources struct {
	Layout      instance.Layout
	Config      *instance.Config
	Definitions *instance.ConfigSet
	Validation  *validate.Report
	Telemetry   *rollup.DB
	// ReadModel is the portal run read model (read.db). Optional: when absent,
	// offline readers and rollback mode use the journal-derived paths.
	// A Reader, deliberately not a *readmodel.Store. §3.1's separation is
	// enforced by the type: the read service holds a handle with no write,
	// backfill, or repair method on it, so a read path that tries to project
	// or reconcile fails to compile rather than being caught in review. That
	// is the whole point of the interface split — reconcileIndex writing to
	// disk from the HTTP list path is how all 40,665 run directories on the
	// live instance came to hold a .lock file.
	ReadModel      readmodel.Reader
	RetentionStats func() readmodel.RetentionStats
	// InstanceLogStats is present only in the live daemon. Dropped appends are
	// process-lifetime state because the failing journal cannot persist its own
	// write failure; offline readers therefore report no journal-health value.
	InstanceLogStats func() journal.InstanceLogStats
	// StorageHealthStats is present only in the live daemon, mirroring
	// InstanceLogStats: tiered low-disk protection's current tier (#4873) is
	// this process's own sampled state, not something an offline reader can
	// reconstruct from the journal.
	StorageHealthStats func() localscheduler.StorageHealthStats
	WorkItemLookup     WorkItemLookup
	SchedulerHeartbeat func() (time.Time, error)
	LivenessTimeout    time.Duration
	// FleetEnrolled reports whether the instance at the given root is
	// associated with a Fleet service (#4218). Optional: NewLocal defaults
	// it to a check against the real Fleet file storage; tests substitute a
	// stub to avoid touching the platform's Fleet storage directory.
	FleetEnrolled func(instanceRoot string) bool
}

// Local reads a tier 1-2 instance's provisioned definitions, journals, and
// telemetry projection.
type Local struct {
	sources LocalSources
	// updateCheck memoizes the parsed <root>/updates/check.json so the
	// highest-frequency route does not re-decode an unchanged file.
	updateCheck      atomic.Pointer[cachedUpdateCheck]
	telemetry        *Telemetry
	ready            func() bool
	now              func() time.Time
	definitions      atomic.Pointer[definitionSnapshot]
	definitionReload atomic.Pointer[DefinitionReloadStatus]
	startupStatus    func() *StartupStatus

	// activeSampler, when non-nil, serves active-run counts from a background
	// sample. Projected services sample read.db; services without a projection
	// retain the historical journal walk.
	activeSampler atomic.Pointer[activeRunSampler]

	// readModelReads gates the read-model list path (§6.6 step 3).
	//
	// Now ON by default. It was off through Waves 2 and early 3 because the
	// store was not continuously current: a run written while the daemon was
	// down was invisible to it, and a fast answer that can silently omit is
	// worse than a slow complete one (§14.7). Three things closed that gap —
	// the projector (#1923) applying intake watermarks, the restart pass
	// covering pending and non-terminal runs, and the bidirectional repair
	// sweep (#1924) reconciling both directions continuously.
	//
	// The old reconcile that used to provide completeness ran on the request
	// path and wrote .lock files into 40,665 run directories; it is deleted.
	readModelReads bool

	// intakeDepth reports how many source watermarks are waiting. Optional; see
	// AttachIntakeDepth.
	intakeDepth intakeDepth

	// projectionHealth reports the projector's apply failures and last drain.
	// Optional; see AttachProjectionHealth.
	projectionHealth func() ProjectionHealth

	// readMode records how this service answers bounded reads (#1933). Empty
	// means projected, which keeps every existing construction unchanged.
	readMode ReadMode

	// instanceLog retains the instance-journal fold behind SchedulerStatus and
	// TimeToFirstPR so those requests read the journal's growth since the last
	// request instead of its whole history (#3050).
	instanceLog instanceFold

	// workflowSchedulerState is the request-safe projection of the small subset
	// of scheduler state used to decorate workflow inventory. HTTP collection
	// reads never hydrate it from the instance journal.
	workflowSchedulerState atomic.Pointer[workflowSchedulerProjection]
	schedulerProjector     atomic.Pointer[schedulerStateProjector]
}

type definitionSnapshot struct {
	set       *instance.ConfigSet
	loadedAt  time.Time
	inventory *inventoryProjection
}

// NewLocal constructs the shared local read service.
func NewLocal(sources LocalSources, ready func() bool) (*Local, error) {
	if ready == nil {
		return nil, fmt.Errorf("read service: readiness function is required")
	}
	if store, ok := sources.ReadModel.(*readmodel.Store); ok && store == nil {
		sources.ReadModel = nil
	}
	if sources.FleetEnrolled == nil {
		sources.FleetEnrolled = defaultFleetEnrolled
	}
	now := time.Now
	snapshot, err := newDefinitionSnapshot(sources.Definitions, sources.Validation, now())
	if err != nil {
		return nil, err
	}
	var telemetry *Telemetry
	if sources.Telemetry != nil {
		telemetry = &Telemetry{store: sources.Telemetry}
	}
	sources.Definitions = nil
	sources.Validation = nil
	local := &Local{
		sources:   sources,
		telemetry: telemetry,
		ready:     ready,
		now:       now,
		// §6.6 step 3: the cutover defaults ON now that the projector and the
		// repair sweep keep the store continuously current. The read model owns
		// every list while enabled, including closed-set refusals.
		readModelReads: sources.ReadModel != nil,
	}
	local.definitions.Store(snapshot)
	return local, nil
}

// StartActiveRunSampler moves the active-run count off the request path.
//
// The daemon calls this; one-shot CLI constructions do not. A projected one-shot
// reader queries read.db directly, while one without a projection pays for the
// authoritative walk once.
//
// interval <= 0 uses the default. Repeated calls reuse the sampler already
// owned by the service. The returned stop function is idempotent and must be
// called on shutdown. It cancels an in-flight walk and returns an error if a
// lower-level filesystem operation does not return within five seconds.
func (s *Local) StartActiveRunSampler(interval time.Duration) func() error {
	sampler := newActiveRunSampler(s.sources.Layout, interval, s.now)
	if s.readModelReads && s.sources.ReadModel != nil {
		sampler.walk = s.projectedActiveRunCounts
	}
	if !s.activeSampler.CompareAndSwap(nil, sampler) {
		sampler = s.activeSampler.Load()
	}
	sampler.Start()
	return sampler.Stop
}

// WaitForInitialActiveRunSample waits for the existing background sampler's first
// result. Daemon startup uses a bounded context before advertising readiness;
// HTTP reads continue to use memory only and never wait or scan on demand.
// Sampling errors are returned, not replaced with an invented zero count.
func (s *Local) WaitForInitialActiveRunSample(ctx context.Context) error {
	sampler := s.activeSampler.Load()
	if sampler == nil {
		return ErrActiveCountsUnavailable
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-sampler.initialSample:
		_, _, err := sampler.Counts()
		return err
	}
}

func (s *Local) projectedActiveRunCounts(ctx context.Context) (map[localscheduler.WorkflowIdentity]int, error) {
	rows, err := s.sources.ReadModel.ActiveRunCounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("read active run projection: %w", err)
	}
	counts := make(map[localscheduler.WorkflowIdentity]int, len(rows))
	for _, row := range rows {
		counts[localscheduler.WorkflowIdentity{
			Gaggle:   row.Gaggle,
			Workflow: row.Workflow,
		}] = row.Count
	}
	return counts, nil
}

// ReloadDefinitions atomically replaces the definitions exposed by the local
// read model after the daemon accepts a config reload.
func (s *Local) ReloadDefinitions(definitions *instance.ConfigSet, validation *validate.Report, loadedAt time.Time) error {
	snapshot, err := newDefinitionSnapshot(definitions, validation, loadedAt)
	if err != nil {
		return err
	}
	s.definitions.Store(snapshot)
	return nil
}

func newDefinitionSnapshot(definitions *instance.ConfigSet, validation *validate.Report, loadedAt time.Time) (*definitionSnapshot, error) {
	if definitions == nil || definitions.Manifest == nil {
		return nil, fmt.Errorf("read service: provisioned manifest is required")
	}
	inventory, err := newInventoryProjection(definitions, validation)
	if err != nil {
		return nil, err
	}
	return &definitionSnapshot{
		set:       definitions,
		loadedAt:  loadedAt.UTC(),
		inventory: inventory,
	}, nil
}

// Health returns daemon readiness, canonical instance identity, and source
// freshness.
func (s *Local) healthUnannotated(ctx context.Context) (Health, error) {
	if err := ctx.Err(); err != nil {
		return Health{}, err
	}
	// #2265: resolve through the generation pointer rather than the legacy
	// bare "events.jsonl" name — that path goes frozen (its mtime stops
	// advancing) the first time in-daemon compaction rotates the journal to
	// a new generation, which would make this freshness check falsely go
	// stale on any instance that has ever compacted.
	eventsPath, err := journal.InstanceEventsPath(s.sources.Layout.SchedulerDir())
	if err != nil {
		return Health{}, fmt.Errorf("resolve instance journal path: %w", err)
	}
	info, err := os.Stat(eventsPath)
	if err != nil {
		return Health{}, fmt.Errorf("read instance journal freshness: %w", err)
	}

	observedAt := s.now().UTC()
	journalUpdatedAt := info.ModTime().UTC()
	definitions := s.definitions.Load()
	ref := definitions.set.Manifest.Spec.Instance
	healthy := true
	var lastSchedulerTickAt *time.Time
	var lastTickAgeMillis *int64
	if s.sources.SchedulerHeartbeat != nil {
		lastTickAt, err := s.sources.SchedulerHeartbeat()
		if err != nil {
			return Health{}, fmt.Errorf("read scheduler heartbeat: %w", err)
		}
		liveness := daemonstate.Evaluate(observedAt, lastTickAt, s.sources.LivenessTimeout)
		healthy = liveness.Healthy
		lastSchedulerTickAt = &liveness.LastTickAt
		ageMillis := liveness.Age.Milliseconds()
		lastTickAgeMillis = &ageMillis
	}

	build := version.Get()
	return Health{
		Update:           s.updateAvailability(),
		DefinitionReload: s.definitionReloadSnapshot(),
		Startup:          s.startupStatusSnapshot(),
		APIVersion:       APIVersion,
		SchemaVersion:    SchemaVersion,
		Build: BuildMetadata{
			Version: build.Version,
			Commit:  build.Commit,
			Date:    build.Date,
		},
		Ready:   s.ready(),
		Healthy: healthy,
		Instance: InstanceIdentity{
			Name:        ref.Name,
			Environment: ref.Environment,
		},
		Freshness: Freshness{
			ObservedAt:          observedAt,
			DefinitionsLoadedAt: definitions.loadedAt,
			JournalUpdatedAt:    &journalUpdatedAt,
			LastSchedulerTickAt: lastSchedulerTickAt,
			LastTickAgeMillis:   lastTickAgeMillis,
		},
	}, nil
}

// updateAvailability reports the daemon's cached release check. It is a local
// read, never a request: the daemon owns the network side and writes the
// answer to <root>/updates/check.json on its own schedule.
//
// Every failure mode returns nil rather than an error. A missing cache is the
// normal state before the first check, and a corrupt one must not take
// /api/v1/health down — losing an advisory field is not worth failing a health
// endpoint over.
func (s *Local) updateAvailability() *UpdateAvailability {
	// An operator who turned the check off must not keep seeing its last
	// answer. The daemon's disabled path never refreshes or removes the cache
	// (there is no check running to do so), so a file left by an earlier
	// enabled run would otherwise assert a pending update forever, with
	// nothing short of editing the filesystem to silence it.
	if !s.sources.Config.UpdateCheckSettings().EnabledEffective() {
		return nil
	}
	result, ok := s.cachedUpdateCheck()
	if !ok {
		return nil
	}
	// The cached verdict is `latest > current` for the build that PERFORMED
	// the check — which is not necessarily the build now serving this
	// response. Refuse a verdict computed against a different binary rather
	// than restate it:
	//
	//   - a `dev` build never refreshes the cache at all (CheckLatest returns
	//     ErrVersionNotComparable before writing), so a file left by a
	//     released build would make a developer's portal claim an update
	//     forever — the exact outcome that early-out exists to prevent;
	//   - after a successful upgrade whose next check cannot complete (no
	//     network, or the check since disabled), the stale file still says the
	//     operator is behind when they are not.
	//
	// `goobers status` tolerates this because it prints the checked-against
	// version and the check's age; a one-line strip states the claim flatly,
	// so the staleness has to be caught here.
	if result.CurrentVersion != version.Get().Version {
		return nil
	}
	return &UpdateAvailability{
		Available:      result.UpdateAvailable,
		LatestVersion:  result.LatestVersion,
		CurrentVersion: result.CurrentVersion,
		Channel:        result.Channel,
		CheckedAt:      result.CheckedAt,
	}
}

// cachedUpdateCheck returns the parsed check, re-reading only when the file
// has actually changed.
//
// /api/v1/health is the highest-frequency route in the service (the portal's
// live poll, three operationalData call sites, `goobers status`, liveness
// probes), and this value changes at most once per updateCheck.interval —
// 24h by default. Decoding the same JSON on every request to learn that
// nothing changed is the kind of request-path I/O this package removes
// elsewhere (definitionReload is served from an atomic.Pointer;
// activeRunSampler exists to move a directory walk off this path). The mtime
// probe rides alongside the os.Stat this method already performs for journal
// freshness.
func (s *Local) cachedUpdateCheck() (selfupdate.CheckResult, bool) {
	info, err := os.Stat(selfupdate.CheckPath(s.sources.Layout.Root))
	if err != nil {
		return selfupdate.CheckResult{}, false
	}
	if cached := s.updateCheck.Load(); cached != nil &&
		cached.modTime.Equal(info.ModTime()) && cached.size == info.Size() {
		return cached.result, cached.valid
	}
	result, err := selfupdate.ReadCheck(s.sources.Layout.Root)
	// A corrupt file is cached as invalid too, so a broken cache is decoded
	// once rather than on every request until someone fixes it.
	s.updateCheck.Store(&cachedUpdateCheck{
		modTime: info.ModTime(), size: info.Size(), result: result, valid: err == nil,
	})
	return result, err == nil
}

// Health returns the read response with its freshness envelope attached.
//
// A thin wrapper around healthUnannotated so the envelope lands on EVERY success
// return rather than on whichever ones someone remembered to edit. Several of
// these methods return successfully from more than one place.
func (s *Local) Health(ctx context.Context) (Health, error) {
	out, err := s.healthUnannotated(ctx)
	if err != nil {
		return Health{}, err
	}
	return annotated[Health](ctx, s, out), nil
}
