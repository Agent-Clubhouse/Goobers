// Package readservice projects provisioned definitions, journals, and
// telemetry into the versioned runtime read contract shared by HTTP and CLI
// adapters.
package readservice

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
}

// Health is the versioned daemon health response.
type Health struct {
	APIVersion    string           `json:"apiVersion"`
	SchemaVersion string           `json:"schemaVersion"`
	Ready         bool             `json:"ready"`
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
}

// LocalSources are the three local projections behind the shared service.
type LocalSources struct {
	Layout      instance.Layout
	Definitions *instance.ConfigSet
	Telemetry   *rollup.DB
}

// Local reads a tier 1-2 instance's provisioned definitions, journals, and
// telemetry projection.
type Local struct {
	sources     LocalSources
	telemetry   *Telemetry
	ready       func() bool
	now         func() time.Time
	definitions atomic.Pointer[definitionSnapshot]
}

type definitionSnapshot struct {
	set      *instance.ConfigSet
	loadedAt time.Time
}

// NewLocal constructs the shared local read service.
func NewLocal(sources LocalSources, ready func() bool) (*Local, error) {
	if ready == nil {
		return nil, fmt.Errorf("read service: readiness function is required")
	}
	now := time.Now
	snapshot, err := newDefinitionSnapshot(sources.Definitions, now())
	if err != nil {
		return nil, err
	}
	var telemetry *Telemetry
	if sources.Telemetry != nil {
		telemetry = &Telemetry{store: sources.Telemetry}
	}
	sources.Definitions = nil
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
func (s *Local) ReloadDefinitions(definitions *instance.ConfigSet, loadedAt time.Time) error {
	snapshot, err := newDefinitionSnapshot(definitions, loadedAt)
	if err != nil {
		return err
	}
	s.definitions.Store(snapshot)
	return nil
}

func newDefinitionSnapshot(definitions *instance.ConfigSet, loadedAt time.Time) (*definitionSnapshot, error) {
	if definitions == nil || definitions.Manifest == nil {
		return nil, fmt.Errorf("read service: provisioned manifest is required")
	}
	return &definitionSnapshot{
		set:      definitions,
		loadedAt: loadedAt.UTC(),
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

	journalUpdatedAt := info.ModTime().UTC()
	definitions := s.definitions.Load()
	ref := definitions.set.Manifest.Spec.Instance

	return Health{
		APIVersion:    APIVersion,
		SchemaVersion: SchemaVersion,
		Ready:         s.ready(),
		Instance: InstanceIdentity{
			Name:        ref.Name,
			Environment: ref.Environment,
		},
		Freshness: Freshness{
			ObservedAt:          s.now().UTC(),
			DefinitionsLoadedAt: definitions.loadedAt,
			JournalUpdatedAt:    &journalUpdatedAt,
		},
	}, nil
}
