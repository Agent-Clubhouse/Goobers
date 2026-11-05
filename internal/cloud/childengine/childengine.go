// Package childengine maps the engine's generated-child admission contract
// onto the cloud control plane's PostgreSQL child custody. It runs
// control-side: workers hold no database credentials and reach it through
// the child-admission plane route (#7879). The engine never imports this
// package; this package imports both.
package childengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// MaxWait is the longest durable child wait an admission may pin.
const MaxWait = 24 * time.Hour

// Custody is the subset of the fenced acceptance store the adapter needs.
// *acceptance.Store implements it.
type Custody interface {
	GetChild(context.Context, triggerqueue.ChildIdentity) (triggerqueue.ChildRecord, error)
	CheckChildParentOpen(context.Context, triggerqueue.ChildParent) error
	ChildStart(context.Context, triggerqueue.ChildIdentity) (triggerqueue.Record, error)
	ChildProposal(context.Context, triggerqueue.ChildIdentity) (triggerqueue.ChildProposal, error)
	KeepChildBlob(context.Context, triggerqueue.ChildIdentity, string, []byte) error
	ChildBlob(context.Context, triggerqueue.ChildIdentity, string) ([]byte, error)
	BindChildEngineStart(context.Context, triggerqueue.ChildIdentity, acceptance.ChildEngineStart) (acceptance.ChildEngineStart, error)
	ChildEngineStart(context.Context, triggerqueue.ChildIdentity) (acceptance.ChildEngineStart, error)
	FenceChildParent(context.Context, triggerqueue.ChildParent, string, time.Time) error
	ChildResult(context.Context, triggerqueue.ChildIdentity) (triggerqueue.ChildResult, error)
	KeepChildResult(context.Context, triggerqueue.ChildRecord, triggerqueue.ChildResult) error
	SetChildState(context.Context, triggerqueue.ChildIdentity, triggerqueue.ChildStateUpdate, time.Time) error
	DispatchEngineChild(context.Context, triggerqueue.ChildIdentity) error
}

// Accepted is the immutable input a freezer may read: the verified handoff,
// the accepted lineage, its original start envelope and the exact source.
type Accepted struct {
	Request journal.ChildHandoffRequest
	Child   triggerqueue.ChildRecord
	Payload []byte
	Source  []byte
}

// Freezer compiles accepted inputs into the frozen manifest. It MUST be a
// pure function of Accepted: retries and lease successors re-run it and must
// produce byte-identical manifests. Live policy may refuse, never rewrite.
type Freezer func(context.Context, Accepted) (engine.FrozenChildManifest, error)

// Service implements engine.ChildAdmissions over Custody.
type Service struct {
	Custody Custody
	Freeze  Freezer
	// Wait pins the durable child wait from acceptance time, at most MaxWait.
	Wait time.Duration
	// Actor attributes parent fences to the control plane.
	Actor string
	Now   func() time.Time
	// Generations, when set, releases the config generation pin each child's
	// acceptance took, once the engine has bound the child (#8085 c).
	Generations GenerationUnpins
	// Journal opens a run's journal read-only from the control journal, for
	// the finalizer to read a child's result evidence.
	Journal func(ctx context.Context, runID string) (journal.RunReader, error)
	// Capacity is the parent capacity plane; nil refuses Park and Complete
	// as unavailable.
	Capacity ParentCapacity
}

// GenerationUnpins releases a child's pinned config generation.
// *objstore.Source implements it.
type GenerationUnpins interface {
	UnpinGeneration(ctx context.Context, generation, owner string) error
}

var _ engine.ChildAdmissions = (*Service)(nil)

func identityOf(r journal.ChildHandoffRequest) triggerqueue.ChildIdentity {
	return triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: r.Gaggle, ParentRunID: r.ParentRunID}, StageOccurrence: r.Origin.StageOccurrence, InvocationKey: r.InvocationKey}
}

func (s *Service) configured() error {
	if s == nil || s.Custody == nil || s.Freeze == nil || s.Wait <= 0 || s.Wait > MaxWait || s.Actor == "" || s.Now == nil {
		return errors.New("childengine: service is not configured")
	}
	return nil
}

// acceptedChild verifies the handoff names exactly one accepted, live lineage
// in an open family.
func (s *Service) acceptedChild(ctx context.Context, r journal.ChildHandoffRequest) (triggerqueue.ChildRecord, error) {
	identity := identityOf(r)
	child, err := s.Custody.GetChild(ctx, identity)
	if err != nil {
		return child, err
	}
	if child.Identity != identity || child.AcceptanceID != r.AcceptanceID || child.RunID != r.ChildRunID || child.ProposalDigest != r.SourceDigest ||
		!child.TombstonedAt.IsZero() || !child.AcknowledgedAt.IsZero() || child.CancellationRequested {
		return child, errNotLiveChild
	}
	return child, s.Custody.CheckChildParentOpen(ctx, identity.ChildParent)
}

func binding(in engine.ChildAdmissionInput, child triggerqueue.ChildRecord, start acceptance.ChildEngineStart) engine.ChildRunBinding {
	return engine.ChildRunBinding{Request: in.Request, Child: child, ManifestDigest: start.ManifestDigest, ParentWorkflow: start.ParentWorkflow,
		ParentWorkflowID: start.ParentWorkflowID, ParentTemporalRunID: start.ParentTemporalRunID, Deadline: start.Deadline}
}

// Admit freezes and binds one child start. A child already bound to another
// parent execution returns that owner's receipt unchanged; the engine refuses
// it as a sealed-child restart rather than re-binding.
func (s *Service) Admit(ctx context.Context, in engine.ChildAdmissionInput) (_ engine.ChildRunBinding, err error) {
	defer func() { err = classify(err) }()
	if err := s.configured(); err != nil {
		return engine.ChildRunBinding{}, err
	}
	child, err := s.acceptedChild(ctx, in.Request)
	if err != nil {
		return engine.ChildRunBinding{}, err
	}
	identity := identityOf(in.Request)
	if start, err := s.Custody.ChildEngineStart(ctx, identity); err == nil {
		return binding(in, child, start), s.unpinBound(ctx, identity, child)
	}
	receipt, err := s.Custody.ChildStart(ctx, identity)
	if err != nil {
		return engine.ChildRunBinding{}, err
	}
	proposal, err := s.Custody.ChildProposal(ctx, identity)
	if err != nil {
		return engine.ChildRunBinding{}, err
	}
	manifest, err := s.Freeze(ctx, Accepted{Request: in.Request, Child: child, Payload: receipt.Payload, Source: proposal.Source})
	if err != nil {
		return engine.ChildRunBinding{}, fmt.Errorf("childengine: freeze accepted child: %w", err)
	}
	// The workflow worker supplies its parent's pinned posture as part of
	// the admission command. Keep it in immutable, digest-bound custody; an
	// applied generation or a later load must never decide this permission.
	manifest.Input.ChildWorkflowsEnabled = in.ChildWorkflowsEnabled
	data, err := json.Marshal(manifest)
	if err != nil {
		return engine.ChildRunBinding{}, err
	}
	digest := journal.Digest(data)
	if err := s.Custody.KeepChildBlob(ctx, identity, digest, data); err != nil {
		return engine.ChildRunBinding{}, err
	}
	start, err := s.Custody.BindChildEngineStart(ctx, identity, acceptance.ChildEngineStart{ManifestDigest: digest, ParentWorkflow: in.ParentWorkflow,
		ParentWorkflowID: in.ParentWorkflowID, ParentTemporalRunID: in.ParentTemporalRunID, Deadline: child.AcceptedAt.Add(s.Wait)})
	if errors.Is(err, acceptance.ErrChildEngineStartOwned) {
		// A concurrent or earlier parent execution won the bind.
		start, err = s.Custody.ChildEngineStart(ctx, identity)
	}
	if err != nil {
		return engine.ChildRunBinding{}, err
	}
	return binding(in, child, start), s.unpin(ctx, child, receipt.Payload)
}

// unpinBound releases the pin of a child an earlier Admit already bound.
func (s *Service) unpinBound(ctx context.Context, identity triggerqueue.ChildIdentity, child triggerqueue.ChildRecord) error {
	if s.Generations == nil {
		return nil
	}
	receipt, err := s.Custody.ChildStart(ctx, identity)
	if err != nil {
		return err
	}
	return s.unpin(ctx, child, receipt.Payload)
}

// unpin releases the generation pin the child's acceptance took: the bound
// manifest is frozen, so the engine no longer needs the generation. A failed
// release is retryable; the engine's retried Admit, already bound, releases
// again, and releasing an absent pin is done.
func (s *Service) unpin(ctx context.Context, child triggerqueue.ChildRecord, payload []byte) error {
	if s.Generations == nil {
		return nil
	}
	envelope, err := childworkflow.DecodeStartEnvelope(payload)
	if err != nil {
		return err
	}
	if err := s.Generations.UnpinGeneration(ctx, envelope.ConfigGeneration, child.AcceptanceID); err != nil {
		return fmt.Errorf("%w: release the child's config generation pin: %w", engine.ErrChildCustodyUnavailable, err)
	}
	return nil
}

// Load reads the manifest the binding's own start intent recorded. A receipt
// for another parent execution or manifest is refused.
func (s *Service) Load(ctx context.Context, b engine.ChildRunBinding) (_ []byte, err error) {
	defer func() { err = classify(err) }()
	if err := s.configured(); err != nil {
		return nil, err
	}
	identity := identityOf(b.Request)
	start, err := s.Custody.ChildEngineStart(ctx, identity)
	if err != nil {
		return nil, err
	}
	if start.ManifestDigest != b.ManifestDigest || start.ParentWorkflowID != b.ParentWorkflowID || start.ParentTemporalRunID != b.ParentTemporalRunID || !start.Deadline.Equal(b.Deadline) {
		return nil, errBindingDiffers
	}
	return s.Custody.ChildBlob(ctx, identity, start.ManifestDigest)
}

// Cancel durably fences the exact parent family: no later admission or
// queued start. It retains every custody record.
func (s *Service) Cancel(ctx context.Context, b engine.ChildRunBinding) (err error) {
	defer func() { err = classify(err) }()
	if err := s.configured(); err != nil {
		return err
	}
	return s.Custody.FenceChildParent(ctx, identityOf(b.Request).ChildParent, s.Actor, s.Now())
}
