package childworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Origin is authenticated transport context, never decoded from a request body.
// GrantID and AttemptID authorize actions; neither changes durable child identity.
type Origin struct {
	GrantID, Gaggle, RunID, StageOccurrence, AttemptID, ConfigDigest, PolicyDigest string
}

// Authority is a trusted, immutable snapshot of the live parent's pinned
// execution definitions intersected with current permissions. Actor must be the
// stable logical principal across replacement attempts, not the grant nonce.
type Authority struct {
	Origin               Origin
	Actor                string
	Admission            AdmissionContext
	ConfigGeneration     string
	ParentWorkflow       string
	ParentWorkflowDigest string
	ParentGooberDigest   string
}

// AuthorityResolver must verify current grant ownership, active attempt,
// occurrence, gaggle, policy, config generation and effective permissions on
// every call. A valid transport signature alone cannot satisfy this contract.
// Resolve returns an independent snapshot; request bodies cannot supply grants.
// The service checks again immediately before acceptance. The trusted launcher
// must also bind and revoke the grant in the queue; acceptance checks that exact
// binding in its transaction. Current policy changes must revoke affected grants
// before publishing the changed runtime authority.
type AuthorityResolver interface {
	Resolve(context.Context, Origin) (Authority, error)
}

// AuthorityResolverFunc adapts a trusted runtime lookup to AuthorityResolver.
type AuthorityResolverFunc func(context.Context, Origin) (Authority, error)

// Resolve delegates to the configured runtime lookup.
func (f AuthorityResolverFunc) Resolve(ctx context.Context, origin Origin) (Authority, error) {
	return f(ctx, origin)
}

// SubmissionRequest contains only authored source and its caller-chosen retry
// key. All scope, identity, policy and credentials come from AuthorityResolver.
type SubmissionRequest struct {
	InvocationKey string
	Source        []byte
}

// Submission is durable custody, not proof that execution or a wait has begun.
type Submission struct {
	Child     triggerqueue.ChildRecord
	Duplicate bool
	Envelope  ChildStartEnvelope
}

// SubmissionService validates and accepts children; it never runs a workflow.
// Its queue is the existing durable trigger store, not a second queue/database.
type SubmissionService struct {
	Queue     *triggerqueue.Store
	Authority AuthorityResolver
	Now       func() time.Time
}

// Submission refusals contain no authored source or other occurrence's data.
var (
	ErrAuthorityUnavailable = errors.New("childworkflow: current stage authority unavailable")
	ErrAuthorityChanged     = errors.New("childworkflow: stage authority changed during operation")
	ErrProposalExpired      = errors.New("childworkflow: retained proposal has expired")
	ErrSubmissionInvalid    = errors.New("childworkflow: invalid submission or retained envelope")
)

// Validate is advisory and writes no custody. It still authenticates the live
// origin before compiling and rechecks authority before returning any result.
func (s *SubmissionService) Validate(ctx context.Context, origin Origin, source []byte) (*Proposal, error) {
	_, proposal, err := s.validated(ctx, origin, source)
	return proposal, err
}

func (s *SubmissionService) validated(ctx context.Context, origin Origin, source []byte) (Authority, *Proposal, error) {
	authority, err := s.resolve(ctx, origin)
	if err != nil {
		return Authority{}, nil, err
	}
	validator, err := NewValidator(authority.Admission)
	if err != nil {
		return Authority{}, nil, err
	}
	proposal, err := validator.Validate(source)
	if err != nil {
		return Authority{}, nil, err
	}
	if proposal.ConfigDigest != origin.ConfigDigest || proposal.PolicyDigest != origin.PolicyDigest {
		return Authority{}, nil, ErrAuthorityUnavailable
	}
	if err := s.recheck(ctx, authority); err != nil {
		return Authority{}, nil, err
	}
	return authority, proposal, nil
}

// Submit repeats trusted validation and atomically retains exact source plus
// lineage/start custody. It neither registers a named workflow nor dispatches.
func (s *SubmissionService) Submit(ctx context.Context, origin Origin, request SubmissionRequest) (Submission, error) {
	if s.Queue == nil || !submissionText(request.InvocationKey, 256) {
		return Submission{}, ErrSubmissionInvalid
	}
	authority, proposal, err := s.validated(ctx, origin, request.Source)
	if err != nil {
		return Submission{}, err
	}
	envelope, err := childStartEnvelope(authority, proposal, request.InvocationKey)
	if err != nil {
		return Submission{}, err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return Submission{}, err
	}
	// All validation and envelope construction precede the final live check.
	// AcceptChild fences parent cancellation and attempt replacement atomically.
	// Runtime integration must additionally pass the expected stage-authority
	// binding into that transaction; resolver rechecking alone is not an atomic
	// attempt/config revocation fence.
	if err := s.recheck(ctx, authority); err != nil {
		return Submission{}, err
	}
	binding, err := s.boundAuthority(ctx, origin)
	if err != nil {
		return Submission{}, err
	}
	child, duplicate, err := s.Queue.AcceptChild(ctx, triggerqueue.ChildAcceptance{
		Identity: envelope.Identity(), Actor: authority.Actor, Payload: payload, Authority: &binding,
		MaxChildren: envelope.MaxChildren,
		Proposal:    &triggerqueue.ChildProposal{Digest: proposal.SourceDigest, Source: proposal.Source},
	}, s.now())
	if err != nil {
		return Submission{}, err
	}
	if !child.TombstonedAt.IsZero() {
		return Submission{}, ErrProposalExpired
	}
	return Submission{Child: child, Duplicate: duplicate, Envelope: envelope}, nil
}

// Get authorizes the current occurrence before every custody read. A caller
// can supply only its invocation key, never another child/run/occurrence ID.
// Missing or tampered retained source fails closed; reads do not repair it.
func (s *SubmissionService) Get(ctx context.Context, origin Origin, invocationKey string) (Submission, error) {
	if s.Queue == nil || !submissionText(invocationKey, 256) {
		return Submission{}, ErrSubmissionInvalid
	}
	authority, err := s.resolve(ctx, origin)
	if err != nil {
		return Submission{}, err
	}
	identity := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: origin.Gaggle, ParentRunID: origin.RunID}, StageOccurrence: origin.StageOccurrence, InvocationKey: invocationKey}
	child, err := s.Queue.GetChild(ctx, identity)
	if err != nil {
		return Submission{}, err
	}
	if !child.TombstonedAt.IsZero() {
		return Submission{}, ErrProposalExpired
	}
	if err := s.recheck(ctx, authority); err != nil {
		return Submission{}, err
	}
	artifact, err := s.Queue.ChildProposal(ctx, identity)
	if err != nil {
		return Submission{}, err
	}
	receipt, err := s.Queue.VerifiedChildStart(ctx, identity, authority.Actor)
	if err != nil {
		return Submission{}, err
	}
	retained, err := DecodeStartEnvelope(receipt.Payload)
	if err != nil {
		return Submission{}, err
	}
	if err := ValidateRetainedCustody(authority, retained, child, artifact); err != nil {
		return Submission{}, err
	}
	if err := s.recheck(ctx, authority); err != nil {
		return Submission{}, err
	}
	return Submission{Child: child, Envelope: retained}, nil
}

func (s *SubmissionService) resolve(ctx context.Context, origin Origin) (Authority, error) {
	if s.Authority == nil || !origin.valid() {
		return Authority{}, ErrAuthorityUnavailable
	}
	authority, err := s.Authority.Resolve(ctx, origin)
	if err != nil {
		return Authority{}, errors.Join(ErrAuthorityUnavailable, err)
	}
	if authority.Origin != origin || authority.Admission.Gaggle.Name != origin.Gaggle || authority.Admission.ConfigDigest != origin.ConfigDigest || !submissionText(authority.Actor, 1024) {
		return Authority{}, ErrAuthorityUnavailable
	}
	for _, value := range []string{authority.ConfigGeneration, authority.ParentWorkflowDigest, authority.ParentGooberDigest} {
		if !blobstore.ValidDigest(value) {
			return Authority{}, ErrAuthorityUnavailable
		}
	}
	if !submissionText(authority.ParentWorkflow, 256) {
		return Authority{}, ErrAuthorityUnavailable
	}
	if _, err := s.boundAuthority(ctx, origin); err != nil {
		return Authority{}, err
	}
	// Never retain a resolver's mutable catalog maps or policy slices.
	raw, err := json.Marshal(authority)
	if err != nil {
		return Authority{}, err
	}
	var snapshot Authority
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return Authority{}, err
	}
	return snapshot, nil
}

// Binding converts authenticated claims for the trusted launcher's queue
// registration. Calling this conversion does not authenticate or register them.
func (o Origin) Binding(expiresAt time.Time) triggerqueue.ChildAuthority {
	return triggerqueue.ChildAuthority{
		ChildParent:     triggerqueue.ChildParent{Gaggle: o.Gaggle, ParentRunID: o.RunID},
		StageOccurrence: o.StageOccurrence, GrantID: o.GrantID, AttemptID: o.AttemptID,
		ConfigDigest: o.ConfigDigest, PolicyDigest: o.PolicyDigest, ExpiresAt: expiresAt,
	}
}

func (s *SubmissionService) boundAuthority(ctx context.Context, origin Origin) (triggerqueue.ChildAuthority, error) {
	if s.Queue == nil {
		return triggerqueue.ChildAuthority{}, ErrAuthorityUnavailable
	}
	stored, err := s.Queue.ChildAuthority(ctx, triggerqueue.ChildParent{Gaggle: origin.Gaggle, ParentRunID: origin.RunID}, origin.StageOccurrence)
	if err != nil {
		return triggerqueue.ChildAuthority{}, errors.Join(ErrAuthorityUnavailable, err)
	}
	expected := origin.Binding(stored.ExpiresAt)
	if err := s.Queue.CheckChildAuthority(ctx, expected, s.now()); err != nil {
		return triggerqueue.ChildAuthority{}, errors.Join(ErrAuthorityUnavailable, err)
	}
	return expected, nil
}

func (s *SubmissionService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *SubmissionService) recheck(ctx context.Context, before Authority) error {
	current, err := s.resolve(ctx, before.Origin)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(current)
	if string(a) != string(b) {
		return ErrAuthorityChanged
	}
	return nil
}

func (o Origin) valid() bool {
	for _, value := range []string{o.GrantID, o.Gaggle, o.RunID, o.StageOccurrence, o.AttemptID} {
		if !submissionText(value, 256) {
			return false
		}
	}
	return len(o.Gaggle) <= 128 && blobstore.ValidDigest(o.ConfigDigest) && blobstore.ValidDigest(o.PolicyDigest)
}

func submissionText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	return strings.IndexFunc(value, unicode.IsControl) == -1
}
