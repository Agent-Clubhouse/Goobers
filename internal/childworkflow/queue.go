package childworkflow

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// ChildQueue is the durable custody the child request path needs: stage
// authority binding, child acceptance and its retained start, proposal and
// disposition. The daemon's trigger store is the first implementation; the
// cloud control plane's PostgreSQL custody is the second (#8085).
type ChildQueue interface {
	AcceptChild(ctx context.Context, req triggerqueue.ChildAcceptance, now time.Time) (triggerqueue.ChildRecord, bool, error)
	GetChild(ctx context.Context, identity triggerqueue.ChildIdentity) (triggerqueue.ChildRecord, error)
	VerifiedChildStart(ctx context.Context, identity triggerqueue.ChildIdentity, actor string) (triggerqueue.Record, error)
	ChildProposal(ctx context.Context, identity triggerqueue.ChildIdentity) (triggerqueue.ChildProposal, error)

	BindChildAuthority(ctx context.Context, grant triggerqueue.ChildAuthority, expectedGrant string, now time.Time) error
	ChildAuthority(ctx context.Context, parent triggerqueue.ChildParent, occurrence string) (triggerqueue.ChildAuthority, error)
	CheckChildAuthority(ctx context.Context, grant triggerqueue.ChildAuthority, now time.Time) error
	CheckChildAuthorityDelivery(ctx context.Context, grant triggerqueue.ChildAuthority, now time.Time) error
	RevokeChildAuthority(ctx context.Context, grant triggerqueue.ChildAuthority) error

	ChildDisposition(ctx context.Context, identity triggerqueue.ChildIdentity) (triggerqueue.ChildDisposition, error)
	RequestChildDisposition(ctx context.Context, request triggerqueue.ChildDispositionRequest, now time.Time) (triggerqueue.ChildDisposition, error)
}

var _ ChildQueue = (*triggerqueue.Store)(nil)
