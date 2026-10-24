package childadmissionclient

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/engine"
)

// BlobPlaneProbeInterval paces re-probing while unverified: each acquire
// probes again at most this often, so a worker that booted before control put
// its canary (or before the blob plane served the object store) recovers
// without a restart.
const BlobPlaneProbeInterval = 30 * time.Second

// ProbedAuthority lends stage attempts child-workflow authority only once
// Probe has shown that the stage blob plane serves control's object store
// (#8085 d): until then every acquire is refused as unavailable, so a
// child-capable stage retries instead of running children whose results its
// parent could never read. Release always passes through.
//
// Verification is sticky: once the canary has been read, the authority stays
// lent for the worker's life. This proves the deployment's wiring (the two
// processes share one store); it is not a health check. A later blob plane
// outage surfaces where it bites, as the executor's fail-closed context
// resolution, which names a retired child result only for an absent receipt
// (childresult.ErrResultRetired), never for an unreachable store.
type ProbedAuthority struct {
	Inner engine.ChildWorkflowAuthority
	// Probe fetches control's canary through the stage blob plane.
	Probe func(context.Context) error
	// Report receives each failed probe; it should log loudly.
	Report func(error)
	Now    func() time.Time

	mu       sync.Mutex
	verified bool
	probedAt time.Time
	last     error
}

var _ engine.ChildWorkflowAuthority = (*ProbedAuthority)(nil)

func (p *ProbedAuthority) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Verify probes now, at boot, and reports the outcome.
func (p *ProbedAuthority) Verify(ctx context.Context) error {
	p.mu.Lock()
	p.probedAt = p.now()
	p.mu.Unlock()
	return p.probe(ctx)
}

// probe runs Probe without holding the lock (it is a network call) and
// records its outcome.
func (p *ProbedAuthority) probe(ctx context.Context) error {
	err := p.Probe(ctx)
	if err != nil {
		err = fmt.Errorf("%w: %w", childresult.ErrBlobPlaneUnverified, err)
		if p.Report != nil {
			p.Report(err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.verified, p.last = true, nil
		return nil
	}
	if !p.verified {
		p.last = err
	}
	return err
}

// AcquireChildAuthority acquires through Inner once the blob plane is verified.
func (p *ProbedAuthority) AcquireChildAuthority(ctx context.Context, request engine.ChildAuthorityRequest) (engine.ChildAuthorityGrant, error) {
	p.mu.Lock()
	verified, last, due := p.verified, p.last, false
	if !verified && (p.probedAt.IsZero() || p.now().Sub(p.probedAt) >= BlobPlaneProbeInterval) {
		// Claim this probe slot, so concurrent acquires do not all probe.
		p.probedAt, due = p.now(), true
	}
	p.mu.Unlock()
	if !verified {
		err := last
		if due {
			err = p.probe(ctx)
		}
		if err != nil {
			return engine.ChildAuthorityGrant{}, fmt.Errorf("%w: %w", engine.ErrChildCustodyUnavailable, err)
		}
	}
	return p.Inner.AcquireChildAuthority(ctx, request)
}

// ReleaseChildAuthority always releases through Inner.
func (p *ProbedAuthority) ReleaseChildAuthority(ctx context.Context, release engine.ChildAuthorityRelease) error {
	return p.Inner.ReleaseChildAuthority(ctx, release)
}
