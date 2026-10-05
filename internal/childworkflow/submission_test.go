package childworkflow

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

type submissionAuthority struct {
	current Authority
	revoked bool
	calls   int
	before  func(int)
}

func (r *submissionAuthority) Resolve(_ context.Context, origin Origin) (Authority, error) {
	r.calls++
	if r.before != nil {
		r.before(r.calls)
	}
	if r.revoked || origin != r.current.Origin {
		return Authority{}, ErrAuthorityUnavailable
	}
	return r.current, nil
}

func submissionFixture(t *testing.T) (*SubmissionService, *submissionAuthority, string) {
	t.Helper()
	input := testContext()
	input.ConfigDigest = digest([]byte("pinned config"))
	proposal, err := validator(t, input).Validate([]byte(validProposal))
	if err != nil {
		t.Fatal(err)
	}
	authority := Authority{
		Origin: Origin{GrantID: "grant-1", Gaggle: "web", RunID: "0123456789abcdef0123456789abcdef", StageOccurrence: "plan/branch-0/visit-1", AttemptID: "attempt-1", ConfigDigest: input.ConfigDigest, PolicyDigest: proposal.PolicyDigest},
		Actor:  "run:0123456789abcdef0123456789abcdef", Admission: input,
		ConfigGeneration: digest([]byte("archive")), ParentWorkflow: "parent", ParentWorkflowDigest: digest([]byte("parent")), ParentGooberDigest: digest([]byte("parent goober")),
	}
	resolver := &submissionAuthority{current: authority}
	path := filepath.Join(t.TempDir(), "accepted.db")
	queue, err := triggerqueue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &SubmissionService{Queue: queue, Authority: resolver, Now: func() time.Time { return time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC) }}
	bindSubmissionOrigin(t, s, authority.Origin, "")
	t.Cleanup(func() { _ = s.Queue.Close() })
	return s, resolver, path
}

func bindSubmissionOrigin(t *testing.T, s *SubmissionService, origin Origin, previous string) {
	t.Helper()
	if err := s.Queue.BindChildAuthority(t.Context(), origin.Binding(s.now().Add(time.Hour)), previous, s.now()); err != nil {
		t.Fatal(err)
	}
}

func submissionDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func proposalCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM child_proposals`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSubmissionRevalidatesAndReopensExactCustody(t *testing.T) {
	s, resolver, path := submissionFixture(t)
	origin := resolver.current.Origin
	source := []byte(validProposal)
	if _, err := s.Validate(t.Context(), origin, source); err != nil {
		t.Fatal(err)
	}
	db := submissionDB(t, path)
	if n := proposalCount(t, db); n != 0 {
		t.Fatalf("advisory validation wrote %d artifacts", n)
	}
	accepted, err := s.Submit(t.Context(), origin, SubmissionRequest{InvocationKey: "call", Source: source})
	if err != nil || accepted.Duplicate {
		t.Fatalf("submit=%+v, %v", accepted, err)
	}
	if accepted.Envelope.Kind != ChildStartKind || accepted.Envelope.SourceDigest != digest(source) || accepted.Envelope.ConfigDigest != origin.ConfigDigest || accepted.Envelope.PolicyDigest != origin.PolicyDigest || accepted.Child.ProposalDigest != digest(source) {
		t.Fatalf("pins=%+v", accepted)
	}
	receipt, err := s.Queue.Get(t.Context(), accepted.Child.AcceptanceID, resolver.current.Actor)
	if err != nil || len(receipt.Payload) > triggerqueue.MaxPayloadBytes || bytes.Contains(receipt.Payload, source) {
		t.Fatalf("ordinary payload contains source or invalid receipt: %v", err)
	}
	if err := s.Queue.Close(); err != nil {
		t.Fatal(err)
	}
	s.Queue, err = triggerqueue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(t.Context(), origin, "call")
	if err != nil || got.Child != accepted.Child || got.Envelope != accepted.Envelope {
		t.Fatalf("reopened status=%+v, %v", got, err)
	}
	// Authentication nonces change across replacement attempts; custody does not.
	resolver.current.Origin.GrantID = "grant-2"
	resolver.current.Origin.AttemptID = "attempt-2"
	bindSubmissionOrigin(t, s, resolver.current.Origin, origin.GrantID)
	origin = resolver.current.Origin
	duplicate, err := s.Submit(t.Context(), origin, SubmissionRequest{InvocationKey: "call", Source: source})
	if err != nil || !duplicate.Duplicate || duplicate.Child.AcceptanceID != accepted.Child.AcceptanceID {
		t.Fatalf("replacement attempt retry=%+v, %v", duplicate, err)
	}
	if n := proposalCount(t, db); n != 1 {
		t.Fatalf("retry stored %d artifacts", n)
	}
	if _, err := s.Submit(t.Context(), origin, SubmissionRequest{InvocationKey: "call", Source: append([]byte("# changed bytes\n"), source...)}); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatalf("changed source retry=%v", err)
	}
}

func TestSubmissionRejectsMissingOrTamperedAcceptedArtifact(t *testing.T) {
	for _, mutation := range []string{"missing", "tampered"} {
		t.Run(mutation, func(t *testing.T) {
			s, resolver, path := submissionFixture(t)
			req := SubmissionRequest{InvocationKey: "call", Source: []byte(validProposal)}
			accepted, err := s.Submit(t.Context(), resolver.current.Origin, req)
			if err != nil {
				t.Fatal(err)
			}
			db := submissionDB(t, path)
			query := `DELETE FROM child_proposals WHERE digest=?`
			if mutation == "tampered" {
				query = `UPDATE child_proposals SET source=X'74616d7065726564' WHERE digest=?`
			}
			if _, err := db.Exec(query, accepted.Envelope.SourceDigest); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Get(t.Context(), resolver.current.Origin, "call"); !errors.Is(err, triggerqueue.ErrChildProposalUnavailable) {
				t.Fatalf("status after %s=%v", mutation, err)
			}
			if _, err := s.Submit(t.Context(), resolver.current.Origin, req); !errors.Is(err, triggerqueue.ErrChildProposalUnavailable) {
				t.Fatalf("retry repaired %s custody: %v", mutation, err)
			}
			if mutation == "missing" && proposalCount(t, db) != 0 {
				t.Fatal("missing accepted artifact was recreated")
			}
		})
	}
}

func TestSubmissionRequiresCurrentAuthorityBeforeCustodyReads(t *testing.T) {
	s, resolver, _ := submissionFixture(t)
	origin := resolver.current.Origin
	if _, err := s.Submit(t.Context(), origin, SubmissionRequest{InvocationKey: "call", Source: []byte(validProposal)}); err != nil {
		t.Fatal(err)
	}
	resolver.revoked = true
	// A closed database proves the denied operation never reaches custody reads.
	if err := s.Queue.Close(); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []func() error{
		func() error { _, err := s.Get(t.Context(), origin, "call"); return err },
		func() error { _, err := s.Validate(t.Context(), origin, []byte(validProposal)); return err },
		func() error {
			_, err := s.Submit(t.Context(), origin, SubmissionRequest{InvocationKey: "call", Source: []byte(validProposal)})
			return err
		},
	} {
		if err := operation(); !errors.Is(err, ErrAuthorityUnavailable) {
			t.Fatalf("revoked action=%v", err)
		}
	}
}

func TestSubmissionRechecksAuthorityImmediatelyBeforeAcceptance(t *testing.T) {
	for _, change := range []string{"revoked", "binding-revoked", "narrowed", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			s, resolver, path := submissionFixture(t)
			resolver.before = func(call int) {
				if call != 3 {
					return
				}
				switch change {
				case "revoked":
					resolver.revoked = true
				case "binding-revoked":
					if err := s.Queue.RevokeChildAuthority(t.Context(), resolver.current.Origin.Binding(s.now().Add(time.Hour))); err != nil {
						t.Fatal(err)
					}
				case "narrowed":
					resolver.current.Admission.GrantedCapabilities = []string{"agent:model"}
				case "cancelled":
					if err := s.Queue.FenceChildParent(t.Context(), triggerqueue.ChildParent{Gaggle: resolver.current.Origin.Gaggle, ParentRunID: resolver.current.Origin.RunID}, "human", s.Now()); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err := s.Submit(t.Context(), resolver.current.Origin, SubmissionRequest{InvocationKey: "call", Source: []byte(validProposal)})
			want := ErrAuthorityUnavailable
			if change == "narrowed" {
				want = ErrAuthorityChanged
			}
			if change == "cancelled" {
				want = triggerqueue.ErrParentCancelled
			}
			if !errors.Is(err, want) {
				t.Fatalf("%s acceptance=%v, want %v", change, err, want)
			}
			db := submissionDB(t, path)
			if n := proposalCount(t, db); n != 0 {
				t.Fatalf("failed acceptance retained %d blobs", n)
			}
			pending, err := s.Queue.Pending(t.Context(), 100)
			if err != nil || len(pending) != 0 {
				t.Fatalf("failed acceptance retained starts: %+v, %v", pending, err)
			}
		})
	}
}

func TestSubmissionCannotUseUnboundOrSupersededGrant(t *testing.T) {
	s, resolver, _ := submissionFixture(t)
	original := resolver.current.Origin
	// Even a mistakenly permissive runtime resolver cannot skip durable launch custody.
	resolver.current.Origin.GrantID = "not-registered"
	_, err := s.Submit(t.Context(), resolver.current.Origin, SubmissionRequest{InvocationKey: "call", Source: []byte(validProposal)})
	if !errors.Is(err, ErrAuthorityUnavailable) {
		t.Fatalf("unbound grant admitted: %v", err)
	}
	replacement := original
	replacement.GrantID, replacement.AttemptID = "replacement", "replacement-attempt"
	bindSubmissionOrigin(t, s, replacement, original.GrantID)
	resolver.current.Origin = original
	if _, err := s.Validate(t.Context(), original, []byte(validProposal)); !errors.Is(err, ErrAuthorityUnavailable) {
		t.Fatalf("superseded attempt validated: %v", err)
	}
}

func TestSubmissionScopeAndOccupiedSlotCannotAccumulateArtifacts(t *testing.T) {
	s, resolver, path := submissionFixture(t)
	origin := resolver.current.Origin
	if _, err := s.Submit(t.Context(), origin, SubmissionRequest{InvocationKey: "call", Source: []byte(validProposal)}); err != nil {
		t.Fatal(err)
	}
	for n := range 5 {
		source := append(bytes.Repeat([]byte("# comment\n"), n+1), []byte(validProposal)...)
		if _, err := s.Submit(t.Context(), origin, SubmissionRequest{InvocationKey: "another", Source: source}); !errors.Is(err, triggerqueue.ErrChildSlotOccupied) {
			t.Fatalf("occupied slot=%v", err)
		}
	}
	db := submissionDB(t, path)
	if n := proposalCount(t, db); n != 1 {
		t.Fatalf("rejected submissions accumulated %d blobs", n)
	}
	foreign := origin
	foreign.Gaggle = "other"
	if _, err := s.Get(t.Context(), foreign, "call"); !errors.Is(err, ErrAuthorityUnavailable) {
		t.Fatalf("foreign gaggle status=%v", err)
	}
	foreign = origin
	foreign.StageOccurrence = "plan/branch-1/visit-1"
	if _, err := s.Get(t.Context(), foreign, "call"); !errors.Is(err, ErrAuthorityUnavailable) {
		t.Fatalf("foreign occurrence status=%v", err)
	}
	resolver.current.Origin = foreign // A genuinely authorized sibling sees only its own namespace.
	bindSubmissionOrigin(t, s, foreign, "")
	if _, err := s.Get(t.Context(), foreign, "call"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("sibling read another occurrence=%v", err)
	}
}
