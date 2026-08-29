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
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

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
}

// Health is the versioned daemon health response.
type Health struct {
	ReadStateEnvelope
	APIVersion    string           `json:"apiVersion"`
	SchemaVersion string           `json:"schemaVersion"`
	Ready         bool             `json:"ready"`
	Healthy       bool             `json:"healthy"`
	Instance      InstanceIdentity `json:"instance"`
	Freshness     Freshness        `json:"freshness"`
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
	ReadModel          readmodel.Reader
	RetentionStats     func() readmodel.RetentionStats
	WorkItemLookup     WorkItemLookup
	SchedulerHeartbeat func() (time.Time, error)
	LivenessTimeout    time.Duration
}

// Local reads a tier 1-2 instance's provisioned definitions, journals, and
// telemetry projection.
type Local struct {
	sources     LocalSources
	telemetry   *Telemetry
	ready       func() bool
	now         func() time.Time
	definitions atomic.Pointer[definitionSnapshot]

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

	// readMode records how this service answers bounded reads (#1933). Empty
	// means projected, which keeps every existing construction unchanged.
	readMode ReadMode
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

	return Health{
		APIVersion:    APIVersion,
		SchemaVersion: SchemaVersion,
		Ready:         s.ready(),
		Healthy:       healthy,
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
