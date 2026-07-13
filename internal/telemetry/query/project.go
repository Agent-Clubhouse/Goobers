package query

import (
	"context"
	"errors"
)

// ErrProjectTelemetryNotConfigured is returned by NotConfiguredSource, the V0
// default ProjectTelemetrySource. Project telemetry (the target product's own
// observability, ARCHITECTURE.md §8) is optional and external — V0 ships this
// interface and the "not configured" behavior; a real client (project ADX or
// whatever the team already runs) is a per-instance, build-time integration
// and explicitly out of this package's scope (#24).
var ErrProjectTelemetryNotConfigured = errors.New("telemetry: project telemetry source is not configured")

// ProjectQueryRequest is a free-form query against an external project
// telemetry source. The shape is intentionally open — a query string plus
// bounded params — since different project stores (ADX, Prometheus, a team's
// own log store) have incompatible native query languages; V0 does not
// standardize one.
type ProjectQueryRequest struct {
	Query  string
	Params map[string]string
}

// ProjectQueryResult is the raw response from a project telemetry source. V0
// does not interpret it; the nomination workflow (#26) that configures a
// concrete source is responsible for shaping/parsing what it gets back.
type ProjectQueryResult struct {
	Data []byte
}

// ProjectTelemetrySource is the seam a producer goober / the nomination
// workflow (#26) uses to read a project's own telemetry — distinct from, and
// never conflated with, the goober-run store this package's rollup queries
// cover (ARCHITECTURE.md §2/§8's two-store doctrine). Concrete
// implementations (a project ADX client at V2, a team's own store at V1+) are
// out of this package's scope; V0 ships only this interface plus
// NotConfiguredSource.
type ProjectTelemetrySource interface {
	Query(ctx context.Context, req ProjectQueryRequest) (ProjectQueryResult, error)
}

// NotConfiguredSource is the V0 default ProjectTelemetrySource: every query
// fails clearly with ErrProjectTelemetryNotConfigured rather than the
// nomination workflow silently getting an empty result it might mistake for
// "no matches."
type NotConfiguredSource struct{}

// Query always returns ErrProjectTelemetryNotConfigured.
func (NotConfiguredSource) Query(context.Context, ProjectQueryRequest) (ProjectQueryResult, error) {
	return ProjectQueryResult{}, ErrProjectTelemetryNotConfigured
}

var _ ProjectTelemetrySource = NotConfiguredSource{}
