// Package readservice projects provisioned definitions, journals, and
// telemetry into the versioned runtime read contract shared by HTTP and CLI
// adapters.
package readservice

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/instance"
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
	TelemetryReader
	ListRuns(context.Context, RunListOptions) (RunList, error)
	GetRun(context.Context, string) (RunDetail, error)
	RunEvents(context.Context, string) (EventList, error)
	StageAttempts(context.Context, string, string) (AttemptList, error)
	Artifact(context.Context, string, string) (ArtifactContent, error)
	Instance(context.Context) (Instance, error)
	Gaggles(context.Context, PageRequest) (GagglePage, error)
	Goobers(context.Context, string, PageRequest) (GooberPage, error)
	Workflows(context.Context, string, PageRequest) (WorkflowPage, error)
	Workflow(context.Context, string, string) (WorkflowDetail, error)
}

// Health is the versioned daemon health response.
type Health struct {
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
	Layout             instance.Layout
	Config             *instance.Config
	Definitions        *instance.ConfigSet
	Validation         *validate.Report
	Telemetry          *rollup.DB
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

	// reconcileMu serializes index reconciliation and, together with
	// lastReconcile, throttles it: a burst of concurrent/rapid ListRuns
	// (the Overview fans out one per phase) collapses to a single directory
	// scan instead of one per request. See reconcileIndex.
	reconcileMu   sync.Mutex
	lastReconcile time.Time
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
	}
	local.definitions.Store(snapshot)
	return local, nil
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
func (s *Local) Health(ctx context.Context) (Health, error) {
	if err := ctx.Err(); err != nil {
		return Health{}, err
	}
	info, err := os.Stat(filepath.Join(s.sources.Layout.SchedulerDir(), "events.jsonl"))
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
