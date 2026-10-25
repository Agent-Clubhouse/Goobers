package childengine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/objstore"
	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
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
	// Adoption records the lease epoch whose predecessors' custody this
	// replica has adopted; it is shared by every call.
	Adoption *EpochAdoption
}

var _ engine.ChildAdmissions = Fenced{}

// EpochAdoption adopts earlier epochs' child custody once per lease epoch,
// before the first call that epoch serves.
type EpochAdoption struct {
	mu      sync.Mutex
	adopted int64
}

func (a *EpochAdoption) ensure(ctx context.Context, store *acceptance.Store, epoch int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.adopted == epoch {
		return nil
	}
	if _, err := store.AdoptStaleChildCustody(ctx); err != nil {
		return err
	}
	a.adopted = epoch
	return nil
}

// Validate reports a missing dependency before the plane is served.
func (f Fenced) Validate() error {
	if f.Pool == nil || f.InstanceID == "" || f.Lease == nil || f.Owner == nil || f.Blobs == nil || f.Objects == nil || f.Applied == nil ||
		f.Wait <= 0 || f.Wait > MaxWait || f.Actor == "" || f.Now == nil || f.Adoption == nil {
		return errors.New("childengine: fenced admissions are not configured")
	}
	return nil
}

// bind returns child custody and the retained generations bound to the live
// lease epoch, after that epoch has adopted every predecessor's custody.
func (f Fenced) bind(ctx context.Context) (*acceptance.Store, *objstore.Source, error) {
	if err := f.Validate(); err != nil {
		return nil, nil, err
	}
	handle := f.Owner()
	if handle == nil {
		return nil, nil, fmt.Errorf("%w: this replica holds no controller lease", engine.ErrChildCustodyUnavailable)
	}
	store, err := acceptance.New(f.Pool, f.InstanceID)
	if err != nil {
		return nil, nil, err
	}
	store.BindFence(f.Lease, handle)
	store.BindChildBlobs(f.Blobs)
	// A new epoch serves only after it owns every predecessor's custody.
	if err := f.Adoption.ensure(ctx, store, handle.Epoch); err != nil {
		return nil, nil, classify(err)
	}
	source, err := objstore.NewGenerationSource(f.Objects, f.InstanceID, "")
	if err != nil {
		return nil, nil, err
	}
	source.BindFence(handle)
	return store, source, nil
}

func (f Fenced) service(ctx context.Context) (*Service, error) {
	store, source, err := f.bind(ctx)
	if err != nil {
		return nil, err
	}
	resolve, err := PinnedResolver(f.InstanceID, source, f.Applied)
	if err != nil {
		return nil, err
	}
	return &Service{Custody: store, Freeze: EngineFreezer(resolve), Wait: f.Wait, Actor: f.Actor, Now: f.Now, Generations: source}, nil
}

// Admit freezes and binds one child start under the live lease.
func (f Fenced) Admit(ctx context.Context, in engine.ChildAdmissionInput) (engine.ChildRunBinding, error) {
	service, err := f.service(ctx)
	if err != nil {
		return engine.ChildRunBinding{}, err
	}
	return service.Admit(ctx, in)
}

// Load reads the bound manifest under the live lease.
func (f Fenced) Load(ctx context.Context, b engine.ChildRunBinding) ([]byte, error) {
	service, err := f.service(ctx)
	if err != nil {
		return nil, err
	}
	return service.Load(ctx, b)
}

// Cancel fences the child family under the live lease.
func (f Fenced) Cancel(ctx context.Context, b engine.ChildRunBinding) error {
	service, err := f.service(ctx)
	if err != nil {
		return err
	}
	return service.Cancel(ctx, b)
}

// FencedAuthority serves engine.ChildWorkflowAuthority from the replica's live
// controller lease epoch, exactly as Fenced serves admissions, and shares its
// epoch adoption.
type FencedAuthority struct {
	OriginTemporal OriginTemporal
	OriginJournal  interface {
		Emit(context.Context, livejournal.EmitRequest) (livejournal.EmitResponse, error)
	}
	Fenced Fenced
	// Journal opens a run's journal read-only from the control-side backend.
	Journal func(ctx context.Context, runID string) (journal.RunReader, error)
	// Execution describes the run's live engine execution.
	Execution func(ctx context.Context, runID string) (startfence.StartedExecution, error)
	// Grants mints child-workflow grants in the credential-grant key domain.
	Grants *podauth.SignedKey
}

var _ engine.ChildWorkflowAuthority = FencedAuthority{}

// Validate reports a missing dependency before the plane is served.
func (a FencedAuthority) Validate() error {
	if err := a.Fenced.Validate(); err != nil {
		return err
	}
	if a.Journal == nil || a.Execution == nil || a.Grants == nil {
		return errors.New("childengine: fenced child authority requires the journal, the engine execution and a grant key")
	}
	return nil
}

// bound returns custody, retained generations and the parent authority, all
// bound to the live lease epoch.
func (a FencedAuthority) bound(ctx context.Context) (*acceptance.Store, *objstore.Source, ParentAuthority, error) {
	if err := a.Validate(); err != nil {
		return nil, nil, ParentAuthority{}, err
	}
	store, source, err := a.Fenced.bind(ctx)
	if err != nil {
		return nil, nil, ParentAuthority{}, err
	}
	parents := ParentAuthority{InstanceID: a.Fenced.InstanceID, Pins: store.RunPinsOf, Journal: a.Journal, Execution: a.Execution,
		StartPending: store.StartPending, Generations: source, Applied: a.Fenced.Applied}
	return store, source, parents, nil
}

func (a FencedAuthority) authorities(ctx context.Context) (Authorities, error) {
	store, _, parents, err := a.bound(ctx)
	if err != nil {
		return Authorities{}, err
	}
	return Authorities{Queue: store, Parents: parents, Grants: a.Grants, Now: a.Fenced.Now}, nil
}

// AcquireChildAuthority binds the attempt's grant under the live lease.
func (a FencedAuthority) AcquireChildAuthority(ctx context.Context, request engine.ChildAuthorityRequest) (engine.ChildAuthorityGrant, error) {
	authorities, err := a.authorities(ctx)
	if err != nil {
		return engine.ChildAuthorityGrant{}, err
	}
	return authorities.AcquireChildAuthority(ctx, request)
}

// ReleaseChildAuthority revokes the named grant under the live lease.
func (a FencedAuthority) ReleaseChildAuthority(ctx context.Context, release engine.ChildAuthorityRelease) error {
	authorities, err := a.authorities(ctx)
	if err != nil {
		return err
	}
	return authorities.ReleaseChildAuthority(ctx, release)
}

// FencedProtocol serves the rest of the engine child protocol (#8085 d):
// parking a parent on its child, sealing the child's result from its
// journal, and resuming the parent with it, under the live lease.
type FencedProtocol struct {
	Fenced Fenced
	// Journal opens a child's journal read-only from the control-side
	// backend, for the finalizer's result evidence.
	Journal func(ctx context.Context, runID string) (journal.RunReader, error)
	// Capacity lends and reacquires the parent's scheduler slot; nil refuses
	// Park and Complete as unavailable until the control scheduler is
	// composed (ErrCapacityPlaneNotComposed).
	Capacity ParentCapacity
}

// Validate reports a missing dependency before the plane is served.
func (p FencedProtocol) Validate() error {
	if err := p.Fenced.Validate(); err != nil {
		return err
	}
	if p.Journal == nil {
		return errors.New("childengine: the fenced child protocol requires the control journal")
	}
	return nil
}

func (p FencedProtocol) service(ctx context.Context) (*Service, error) {
	service, err := p.Fenced.service(ctx)
	if err != nil {
		return nil, err
	}
	service.Journal, service.Capacity = p.Journal, p.Capacity
	return service, nil
}

// Park lends the parked parent's capacity while it waits on its child.
func (p FencedProtocol) Park(ctx context.Context, in engine.ChildParkInput) error {
	service, err := p.service(ctx)
	if err != nil {
		return err
	}
	return service.Park(ctx, in)
}

// Finalize seals the child's terminal result from its journal.
func (p FencedProtocol) Finalize(ctx context.Context, in engine.ChildFinalizeInput) (childresult.TerminalReturn, error) {
	service, err := p.service(ctx)
	if err != nil {
		return childresult.TerminalReturn{}, err
	}
	return service.Finalize(ctx, in)
}

// Complete resumes the parent with exactly the sealed result.
func (p FencedProtocol) Complete(ctx context.Context, in engine.ChildCompletionInput) (apiv1.ContextPointer, error) {
	service, err := p.service(ctx)
	if err != nil {
		return apiv1.ContextPointer{}, err
	}
	return service.Complete(ctx, in)
}

// ObserveChildHandoff reads accepted custody under the current controller lease.
func (a FencedAuthority) ObserveChildHandoff(ctx context.Context, request engine.ChildHandoffObservation) (*journal.ChildHandoffRequest, error) {
	authorities, err := a.authorities(ctx)
	if err != nil {
		return nil, err
	}
	return authorities.ObserveChildHandoff(ctx, request)
}
