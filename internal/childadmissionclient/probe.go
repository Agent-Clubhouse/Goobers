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
// parent could never read. Once verified it stays verified. Release always
// passes through.
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

// Verify probes now, at boot, and reports the outcome.
func (p *ProbedAuthority) Verify(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probeLocked(ctx)
}

func (p *ProbedAuthority) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *ProbedAuthority) probeLocked(ctx context.Context) error {
	p.probedAt = p.now()
	err := p.Probe(ctx)
	if err == nil {
		p.verified, p.last = true, nil
		return nil
	}
	p.last = fmt.Errorf("%w: %w", childresult.ErrBlobPlaneUnverified, err)
	if p.Report != nil {
		p.Report(p.last)
	}
	return p.last
}

// AcquireChildAuthority acquires through Inner once the blob plane is verified.
func (p *ProbedAuthority) AcquireChildAuthority(ctx context.Context, request engine.ChildAuthorityRequest) (engine.ChildAuthorityGrant, error) {
	p.mu.Lock()
	err := error(nil)
	if !p.verified {
		err = p.last
		if p.probedAt.IsZero() || p.now().Sub(p.probedAt) >= BlobPlaneProbeInterval {
			err = p.probeLocked(ctx)
		}
	}
	p.mu.Unlock()
	if err != nil {
		return engine.ChildAuthorityGrant{}, fmt.Errorf("%w: %w", engine.ErrChildCustodyUnavailable, err)
	}
	return p.Inner.AcquireChildAuthority(ctx, request)
}

// ReleaseChildAuthority always releases through Inner.
func (p *ProbedAuthority) ReleaseChildAuthority(ctx context.Context, release engine.ChildAuthorityRelease) error {
	return p.Inner.ReleaseChildAuthority(ctx, release)
}
