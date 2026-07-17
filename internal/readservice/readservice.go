// Package readservice projects provisioned definitions, journals, and
// telemetry into the versioned runtime read contract shared by HTTP and CLI
// adapters.
package readservice

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	sources             LocalSources
	telemetry           *Telemetry
	identity            InstanceIdentity
	ready               func() bool
	now                 func() time.Time
	definitionsLoadedAt time.Time
}

// NewLocal constructs the shared local read service.
func NewLocal(sources LocalSources, ready func() bool) (*Local, error) {
	if sources.Definitions == nil || sources.Definitions.Manifest == nil {
		return nil, fmt.Errorf("read service: provisioned manifest is required")
	}
	if ready == nil {
		return nil, fmt.Errorf("read service: readiness function is required")
	}
	now := time.Now
	ref := sources.Definitions.Manifest.Spec.Instance
	var telemetry *Telemetry
	if sources.Telemetry != nil {
		telemetry = &Telemetry{store: sources.Telemetry}
	}
	return &Local{
		sources:   sources,
		telemetry: telemetry,
		identity: InstanceIdentity{
			Name:        ref.Name,
			Environment: ref.Environment,
		},
		ready:               ready,
		now:                 now,
		definitionsLoadedAt: now().UTC(),
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

	return Health{
		APIVersion:    APIVersion,
		SchemaVersion: SchemaVersion,
		Ready:         s.ready(),
		Instance:      s.identity,
		Freshness: Freshness{
			ObservedAt:          s.now().UTC(),
			DefinitionsLoadedAt: s.definitionsLoadedAt,
			JournalUpdatedAt:    &journalUpdatedAt,
		},
	}, nil
}
