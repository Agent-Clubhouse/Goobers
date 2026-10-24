package claims

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/httpapi"
)

// Follow-up issues for the claims-plane features the cloud refuses.
const (
	// ProviderCleanupIssue tracks provider-cleanup release (cleanup custody
	// and server-side marker removal) in the cloud claims plane.
	ProviderCleanupIssue = 8093
)

// legacyNamespace is the namespace the scheduler-side Store gives unscoped
// claims. A live legacy claim is exclusive against every namespace.
const legacyNamespace = "legacy"

// Plane is the cloud claims plane (httpapi.ClaimService) over control.claim
// and control.claim_history. Every write runs in one controller-lease fenced
// transaction under the instance's claims advisory lock. A row written by an
// earlier epoch is adopted by the write that touches it: authority moved, the
// claim did not, so the predicate is owner_epoch <= epoch and the write stamps
// the current epoch.
type Plane struct {
	pool         *pgxpool.Pool
	instanceID   string
	lease        *cloudlease.Manager
	owner        func() *cloudlease.Handle
	defaultLease time.Duration
	// releaseMarker performs provider-cleanup releases; nil refuses them.
	releaseMarker MarkerReleaser
	shared        SharedResolver
}

// PlaneConfig wires a Plane to the controller pool and the live lease.
type PlaneConfig struct {
	// Pool is the controller role's pool; reads run on it unfenced.
	Pool       *pgxpool.Pool
	InstanceID string
	// Lease fences writes; it must manage the controller lease.
	Lease *cloudlease.Manager
	// Owner returns the live controller handle, or nil while authority is
	// not held (startup, takeover, shutdown).
	Owner func() *cloudlease.Handle
	// DefaultLease applies to acquire and renew requests without leaseSeconds.
	DefaultLease time.Duration
	// ReleaseMarker removes provider claim markers for provider-cleanup
	// releases. Nil (no provider credentials) refuses them with a 503 naming
	// the issue that composes it.
	ReleaseMarker MarkerReleaser
	// Shared resolves the immutable run policy and provider binding. Nil keeps local-only admission.
	Shared SharedResolver
}

var (
	_ httpapi.ClaimService             = (*Plane)(nil)
	_ httpapi.ClaimVerificationService = (*Plane)(nil)
)

// NewPlane validates the configuration and constructs a Plane.
func NewPlane(config PlaneConfig) (*Plane, error) {
	if config.Pool == nil || strings.TrimSpace(config.InstanceID) == "" ||
		config.Lease == nil || config.Owner == nil || config.DefaultLease <= 0 {
		return nil, errors.New("cloud claims plane requires a pool, instance, lease, owner and default lease")
	}
	return &Plane{
		pool: config.Pool, instanceID: config.InstanceID, lease: config.Lease,
		owner: config.Owner, defaultLease: config.DefaultLease, releaseMarker: config.ReleaseMarker, shared: config.Shared,
	}, nil
}

// fenced runs write under the live controller fence and the instance claims
// lock, passing the epoch every row predicate and stamp must use.
func (p *Plane) fenced(ctx context.Context, write func(context.Context, pgx.Tx, int64) error) error {
	return p.fencedWith(ctx, p.owner(), write)
}

// fencedWith is fenced on an explicit handle, for the epoch services that
// hold the authority they act under.
func (p *Plane) fencedWith(ctx context.Context, handle *cloudlease.Handle, write func(context.Context, pgx.Tx, int64) error) error {
	if handle == nil {
		return errAuthorityMoving(nil)
	}
	if handle.InstanceID != p.instanceID {
		return errors.New("cloud claims plane: lease handle belongs to another instance")
	}
	err := p.lease.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockClaims(ctx, tx, p.instanceID); err != nil {
			return err
		}
		return write(ctx, tx, handle.Epoch)
	})
	return fenceError(err)
}

// fenceError maps lease failures to the plane's retry contract. A lost commit
// outcome may have committed, so it is an ambiguous budget overrun
// (request_budget_exceeded), never a safe replay. A stale or self-fenced
// handle aborted before commit, so the request had no effect and the client
// may replay it (503 recovering, the code planehttp treats as refused early).
// A fenced-transaction timeout also aborted before commit; it reports the
// budget overrun.
func fenceError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, cloudlease.ErrCommitUnknown):
		return httpapi.NewInterventionError(http.StatusServiceUnavailable, "request_budget_exceeded",
			"claims write lost its commit outcome; it may have committed", err)
	case errors.Is(err, cloudlease.ErrStale), errors.Is(err, cloudlease.ErrSelfFenced):
		return errAuthorityMoving(err)
	case errors.Is(err, cloudlease.ErrFencedTransactionTimeout):
		return httpapi.NewInterventionError(http.StatusServiceUnavailable, "request_budget_exceeded",
			"claims write exceeded the fenced transaction budget", err)
	}
	return err
}

func errAuthorityMoving(cause error) error {
	return httpapi.NewInterventionError(http.StatusServiceUnavailable, "recovering",
		"claims authority is moving to a new controller epoch; retry", cause)
}

func errProviderCleanupUnavailable() error {
	return httpapi.NewInterventionError(http.StatusServiceUnavailable, "claims_provider_cleanup_unavailable",
		fmt.Sprintf("provider-cleanup release needs provider credentials in the control role (#%d)", ProviderCleanupIssue), nil)
}

// errReleasePending is the ledger's ErrClaimReleasePending: an ordinary
// release must not discard cleanup custody; the provider-cleanup release that
// took it finishes it.
func errReleasePending() error {
	return httpapi.NewInterventionError(http.StatusConflict, "claim_release_pending",
		"claim provider release is pending; retry provider cleanup", nil)
}

func invalidRequest(message string) error {
	return httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest, message, nil)
}

func (p *Plane) leaseDuration(request httpapi.ClaimRequest) (time.Duration, error) {
	switch {
	case request.LeaseSeconds < 0:
		return 0, httpapi.NewInterventionError(http.StatusBadRequest, "invalid_lease", "leaseSeconds must not be negative", nil)
	case request.LeaseSeconds > httpapi.MaxClaimLeaseSeconds:
		return 0, httpapi.NewInterventionError(http.StatusBadRequest, "invalid_lease",
			fmt.Sprintf("leaseSeconds must not exceed %d", httpapi.MaxClaimLeaseSeconds), nil)
	case request.LeaseSeconds == 0:
		return p.defaultLease, nil
	}
	return time.Duration(request.LeaseSeconds) * time.Second, nil
}

// requireKey refuses requests outside a scoped namespace: the HTTP plane has
// no legacy unscoped claims, and the legacy namespace is the scheduler's.
func requireKey(request httpapi.ClaimRequest) error {
	for _, value := range []string{request.Gaggle, request.Provider, request.ItemID, request.RunID} {
		if strings.TrimSpace(value) == "" {
			return invalidRequest("gaggle, provider, itemId and runId are required")
		}
	}
	if request.Gaggle == legacyNamespace && request.Provider == legacyNamespace {
		return invalidRequest("the legacy claim namespace is not available over the claims plane")
	}
	return nil
}
