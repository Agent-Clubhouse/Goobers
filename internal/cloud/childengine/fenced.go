package childengine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/objstore"
	"github.com/goobers/goobers/internal/engine"
)

// Fenced serves engine.ChildAdmissions from whichever controller lease epoch
// this replica currently holds. Every call binds a fresh acceptance store and
// generation source to the live lease handle, so a write can never commit
// under an epoch the replica has lost. With no live lease (a standby or a
// failover in progress) the call is unavailable and the engine retries.
type Fenced struct {
	Pool       *pgxpool.Pool
	InstanceID string
	Lease      *cloudlease.Manager
	Owner      func() *cloudlease.Handle
	Blobs      blobstore.Store
	Objects    *objstore.Metadata
	Applied    AppliedConfiguration
	Wait       time.Duration
	Actor      string
	Now        func() time.Time
}

var _ engine.ChildAdmissions = Fenced{}

// Validate reports a missing dependency before the plane is served.
func (f Fenced) Validate() error {
	if f.Pool == nil || f.InstanceID == "" || f.Lease == nil || f.Owner == nil || f.Blobs == nil || f.Objects == nil || f.Applied == nil ||
		f.Wait <= 0 || f.Wait > MaxWait || f.Actor == "" || f.Now == nil {
		return errors.New("childengine: fenced admissions are not configured")
	}
	return nil
}

func (f Fenced) service() (*Service, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	handle := f.Owner()
	if handle == nil {
		return nil, fmt.Errorf("%w: this replica holds no controller lease", engine.ErrChildCustodyUnavailable)
	}
	store, err := acceptance.New(f.Pool, f.InstanceID)
	if err != nil {
		return nil, err
	}
	store.BindFence(f.Lease, handle)
	store.BindChildBlobs(f.Blobs)
	source, err := objstore.NewGenerationSource(f.Objects, f.InstanceID, "")
	if err != nil {
		return nil, err
	}
	source.BindFence(handle)
	resolve, err := PinnedResolver(f.InstanceID, source, f.Applied)
	if err != nil {
		return nil, err
	}
	return &Service{Custody: store, Freeze: EngineFreezer(resolve), Wait: f.Wait, Actor: f.Actor, Now: f.Now}, nil
}

// Admit freezes and binds one child start under the live lease.
func (f Fenced) Admit(ctx context.Context, in engine.ChildAdmissionInput) (engine.ChildRunBinding, error) {
	service, err := f.service()
	if err != nil {
		return engine.ChildRunBinding{}, err
	}
	return service.Admit(ctx, in)
}

// Load reads the bound manifest under the live lease.
func (f Fenced) Load(ctx context.Context, b engine.ChildRunBinding) ([]byte, error) {
	service, err := f.service()
	if err != nil {
		return nil, err
	}
	return service.Load(ctx, b)
}

// Cancel fences the child family under the live lease.
func (f Fenced) Cancel(ctx context.Context, b engine.ChildRunBinding) error {
	service, err := f.service()
	if err != nil {
		return err
	}
	return service.Cancel(ctx, b)
}
