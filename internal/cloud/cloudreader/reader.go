// Package cloudreader is the control plane's read service: a rootless
// readservice.Local over the PostgreSQL journal and read model, rebuilt on
// every applied configuration generation (#7818 R3). Before the first
// generation is applied it answers every read with readservice.ErrUnavailable
// (503); it never serves a definition-less or empty view.
package cloudreader

import (
	"context"
	"errors"
	"fmt"
	"sync"

	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

// AppliedConfiguration reports the control plane's running generation.
// *cloudconfig.Configuration implements it.
type AppliedConfiguration interface {
	Applied() (cloudconfig.Candidate, bool)
}

// Config is the Reader's sources. Journal, ReadModel, Applied and Ready are
// required; ClaimSnapshot and ProjectionHealth are optional.
type Config struct {
	Journal   journal.ReaderBackend
	ReadModel readmodel.Reader
	Applied   AppliedConfiguration
	// ClaimSnapshot feeds the active-claims view and run claim decoration.
	// Without it both report claims unavailable.
	ClaimSnapshot    func(context.Context) (readservice.ClaimView, error)
	ProjectionHealth func() readservice.ProjectionHealth
	Ready            func() bool
}

// Reader is the generation-swapping control-plane read service.
type Reader struct {
	cfg Config

	mu     sync.Mutex
	digest string
	local  *readservice.Local
}

var _ readservice.Reader = (*Reader)(nil)

// ClaimsReader is a Reader that also serves the active-claims view: it is
// returned only when a claim source is configured, so an API composed without
// one reports the route unconfigured instead of an empty list.
type ClaimsReader struct{ *Reader }

var _ readservice.ActiveClaimsReader = ClaimsReader{}

// ActiveClaims serves the configured claim ledger.
func (r ClaimsReader) ActiveClaims(ctx context.Context) (readservice.ActiveClaimList, error) {
	return call(r.Reader, func(l *readservice.Local) (readservice.ActiveClaimList, error) { return l.ActiveClaims(ctx) })
}

// New returns the read service for cfg: a ClaimsReader when a claim source is
// configured, else a plain Reader.
func New(cfg Config) (readservice.Reader, error) {
	if cfg.Journal == nil || cfg.ReadModel == nil || cfg.Applied == nil || cfg.Ready == nil {
		return nil, errors.New("cloudreader: journal, read model, applied configuration and readiness are required")
	}
	reader := &Reader{cfg: cfg}
	if cfg.ClaimSnapshot != nil {
		return ClaimsReader{reader}, nil
	}
	return reader, nil
}

// current returns the service for the applied generation, rebuilding it when
// the generation changed.
func (r *Reader) current() (*readservice.Local, error) {
	candidate, ok := r.cfg.Applied.Applied()
	if !ok || candidate.Config == nil || candidate.Set == nil {
		return nil, fmt.Errorf("%w: no configuration generation is applied yet", readservice.ErrUnavailable)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.local != nil && r.digest == candidate.Digest {
		return r.local, nil
	}
	local, err := readservice.NewLocal(readservice.LocalSources{
		Rootless: true, Config: candidate.Config, Definitions: candidate.Set,
		Journal: r.cfg.Journal, ReadModel: r.cfg.ReadModel, ClaimSnapshot: r.cfg.ClaimSnapshot,
	}, r.cfg.Ready)
	if err != nil {
		return nil, fmt.Errorf("cloudreader: build read service for generation %s: %w", candidate.Digest, err)
	}
	if r.cfg.ProjectionHealth != nil {
		local.AttachProjectionHealth(r.cfg.ProjectionHealth)
	}
	r.digest, r.local = candidate.Digest, local
	return local, nil
}

func call[T any](r *Reader, read func(*readservice.Local) (T, error)) (T, error) {
	local, err := r.current()
	if err != nil {
		var zero T
		return zero, err
	}
	return read(local)
}

// Health implements readservice.Reader.
func (r *Reader) Health(ctx context.Context) (readservice.Health, error) {
	return call(r, func(l *readservice.Local) (readservice.Health, error) { return l.Health(ctx) })
}

// PortalConfig implements readservice.Reader.
func (r *Reader) PortalConfig(ctx context.Context) (readservice.PortalConfig, error) {
	return call(r, func(l *readservice.Local) (readservice.PortalConfig, error) { return l.PortalConfig(ctx) })
}

// TelemetryCosts implements readservice.Reader.
func (r *Reader) TelemetryCosts(ctx context.Context, req readservice.TelemetryCostRequest) (readservice.TelemetryCostResult, error) {
	return call(r, func(l *readservice.Local) (readservice.TelemetryCostResult, error) { return l.TelemetryCosts(ctx, req) })
}

// TelemetryStats implements readservice.Reader.
func (r *Reader) TelemetryStats(ctx context.Context, req readservice.TelemetryStatsRequest) (readservice.TelemetryStatsResult, error) {
	return call(r, func(l *readservice.Local) (readservice.TelemetryStatsResult, error) {
		return l.TelemetryStats(ctx, req)
	})
}

// TelemetryAttribution implements readservice.Reader.
func (r *Reader) TelemetryAttribution(ctx context.Context, req readservice.TelemetryAttributionRequest) (readservice.TelemetryAttributionResult, error) {
	return call(r, func(l *readservice.Local) (readservice.TelemetryAttributionResult, error) {
		return l.TelemetryAttribution(ctx, req)
	})
}

// TelemetryErrorSignatures implements readservice.Reader.
func (r *Reader) TelemetryErrorSignatures(ctx context.Context, req readservice.TelemetryErrorSignaturesRequest) (readservice.TelemetryErrorSignaturesResult, error) {
	return call(r, func(l *readservice.Local) (readservice.TelemetryErrorSignaturesResult, error) {
		return l.TelemetryErrorSignatures(ctx, req)
	})
}

// TelemetryErrors implements readservice.Reader.
func (r *Reader) TelemetryErrors(ctx context.Context, req readservice.TelemetryErrorsRequest) (readservice.TelemetryErrorsPage, error) {
	return call(r, func(l *readservice.Local) (readservice.TelemetryErrorsPage, error) {
		return l.TelemetryErrors(ctx, req)
	})
}

// TelemetryImplementationOutcomes implements readservice.Reader.
func (r *Reader) TelemetryImplementationOutcomes(ctx context.Context, req readservice.TelemetryImplementationOutcomesRequest) (readservice.TelemetryImplementationOutcomesResult, error) {
	return call(r, func(l *readservice.Local) (readservice.TelemetryImplementationOutcomesResult, error) {
		return l.TelemetryImplementationOutcomes(ctx, req)
	})
}

// ListRuns implements readservice.Reader.
func (r *Reader) ListRuns(ctx context.Context, options readservice.RunListOptions) (readservice.RunList, error) {
	return call(r, func(l *readservice.Local) (readservice.RunList, error) { return l.ListRuns(ctx, options) })
}

// GetRun implements readservice.Reader.
func (r *Reader) GetRun(ctx context.Context, runID string) (readservice.RunDetail, error) {
	return call(r, func(l *readservice.Local) (readservice.RunDetail, error) { return l.GetRun(ctx, runID) })
}

// GetRunWithContinuations serves paged run continuations.
func (r *Reader) GetRunWithContinuations(ctx context.Context, runID, after string) (readservice.RunDetail, error) {
	return call(r, func(l *readservice.Local) (readservice.RunDetail, error) {
		return l.GetRunWithContinuations(ctx, runID, after)
	})
}

// RunChildren implements readservice.Reader.
func (r *Reader) RunChildren(ctx context.Context, runID, cursor string) (readservice.ChildHistoryPage, error) {
	return call(r, func(l *readservice.Local) (readservice.ChildHistoryPage, error) {
		return l.RunChildren(ctx, runID, cursor)
	})
}

// RunEvents implements readservice.Reader.
func (r *Reader) RunEvents(ctx context.Context, runID string) (readservice.EventList, error) {
	return call(r, func(l *readservice.Local) (readservice.EventList, error) { return l.RunEvents(ctx, runID) })
}

// StageAttempts implements readservice.Reader.
func (r *Reader) StageAttempts(ctx context.Context, runID, stage string) (readservice.AttemptList, error) {
	return call(r, func(l *readservice.Local) (readservice.AttemptList, error) { return l.StageAttempts(ctx, runID, stage) })
}

// AddressableAgents implements readservice.Reader.
func (r *Reader) AddressableAgents(ctx context.Context, runID string) ([]readservice.AddressableAgent, error) {
	return call(r, func(l *readservice.Local) ([]readservice.AddressableAgent, error) {
		return l.AddressableAgents(ctx, runID)
	})
}

// ResolveAgentAddress implements readservice.Reader.
func (r *Reader) ResolveAgentAddress(ctx context.Context, runID, address string) (readservice.AgentResolution, error) {
	return call(r, func(l *readservice.Local) (readservice.AgentResolution, error) {
		return l.ResolveAgentAddress(ctx, runID, address)
	})
}

// Artifact implements readservice.Reader.
func (r *Reader) Artifact(ctx context.Context, runID, digest string) (readservice.ArtifactContent, error) {
	return call(r, func(l *readservice.Local) (readservice.ArtifactContent, error) { return l.Artifact(ctx, runID, digest) })
}

// Transcript implements readservice.Reader.
func (r *Reader) Transcript(ctx context.Context, runID string, seq uint64) (readservice.TranscriptContent, error) {
	return call(r, func(l *readservice.Local) (readservice.TranscriptContent, error) {
		return l.Transcript(ctx, runID, seq)
	})
}

// Instance implements readservice.Reader.
func (r *Reader) Instance(ctx context.Context) (readservice.Instance, error) {
	return call(r, func(l *readservice.Local) (readservice.Instance, error) { return l.Instance(ctx) })
}

// Gaggles implements readservice.Reader.
func (r *Reader) Gaggles(ctx context.Context, page readservice.PageRequest) (readservice.GagglePage, error) {
	return call(r, func(l *readservice.Local) (readservice.GagglePage, error) { return l.Gaggles(ctx, page) })
}

// Goobers implements readservice.Reader.
func (r *Reader) Goobers(ctx context.Context, gaggle string, page readservice.PageRequest) (readservice.GooberPage, error) {
	return call(r, func(l *readservice.Local) (readservice.GooberPage, error) { return l.Goobers(ctx, gaggle, page) })
}

// Workflows implements readservice.Reader.
func (r *Reader) Workflows(ctx context.Context, gaggle string, page readservice.PageRequest) (readservice.WorkflowPage, error) {
	return call(r, func(l *readservice.Local) (readservice.WorkflowPage, error) { return l.Workflows(ctx, gaggle, page) })
}

// Connections implements readservice.Reader.
func (r *Reader) Connections(ctx context.Context, gaggle string) (readservice.GaggleConnections, error) {
	return call(r, func(l *readservice.Local) (readservice.GaggleConnections, error) { return l.Connections(ctx, gaggle) })
}

// Workflow implements readservice.Reader.
func (r *Reader) Workflow(ctx context.Context, gaggle, workflow string) (readservice.WorkflowDetail, error) {
	return call(r, func(l *readservice.Local) (readservice.WorkflowDetail, error) {
		return l.Workflow(ctx, gaggle, workflow)
	})
}

// QueueEligibility implements readservice.Reader.
func (r *Reader) QueueEligibility(ctx context.Context, gaggle, workflow string) (readservice.QueueEligibilityView, error) {
	return call(r, func(l *readservice.Local) (readservice.QueueEligibilityView, error) {
		return l.QueueEligibility(ctx, gaggle, workflow)
	})
}

// RunGaggles is the read-model lookup Contains needs.
type RunGaggles interface {
	GetRun(context.Context, string) (readmodel.RunRow, bool, error)
}

// Contains reports whether runID is a run of gaggle, for the trigger plane's
// pod containment check (#8101): a stage pod may only trigger into its own
// run's gaggle. An unknown run is false; a read-model failure is
// ErrUnavailable, never a guess.
func Contains(runs RunGaggles) func(context.Context, string, string) (bool, error) {
	return func(ctx context.Context, gaggle, runID string) (bool, error) {
		if runs == nil {
			return false, fmt.Errorf("%w: no run read model", readservice.ErrUnavailable)
		}
		row, found, err := runs.GetRun(ctx, runID)
		if err != nil {
			return false, fmt.Errorf("%w: look up run gaggle: %w", readservice.ErrUnavailable, err)
		}
		return found && row.Gaggle == gaggle, nil
	}
}
