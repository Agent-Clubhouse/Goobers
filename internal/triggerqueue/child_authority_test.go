package triggerqueue

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func childAuthorityFixture(req ChildAcceptance) ChildAuthority {
	return ChildAuthority{ChildParent: req.Identity.ChildParent, StageOccurrence: req.Identity.StageOccurrence,
		GrantID: "grant-a", AttemptID: "attempt-a", ConfigDigest: "sha256:" + strings.Repeat("a", 64),
		PolicyDigest: "sha256:" + strings.Repeat("b", 64), ExpiresAt: childTestTime.Add(time.Hour)}
}

func TestChildAuthoritySupersessionAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "starts.db")
	s := openTestStore(t, path)
	req := childRequest("parent", "work/visit-1", "call")
	a := childAuthorityFixture(req)
	req.Authority = &a
	if _, _, err := s.AcceptChild(t.Context(), req, childTestTime); !errors.Is(err, ErrChildAuthorityChanged) {
		t.Fatalf("unregistered authority admitted: %v", err)
	}
	if err := s.BindChildAuthority(t.Context(), a, "", childTestTime); err != nil {
		t.Fatal(err)
	}
	child := acceptChildTest(t, s, req, childTestTime)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	if err := s.CheckChildAuthority(t.Context(), a, childTestTime); err != nil {
		t.Fatalf("binding did not survive restart: %v", err)
	}
	b := a
	b.GrantID, b.AttemptID = "grant-b", "attempt-b"
	if err := s.BindChildAuthority(t.Context(), b, "wrong", childTestTime); !errors.Is(err, ErrChildAuthorityChanged) {
		t.Fatalf("missing CAS accepted: %v", err)
	}
	if err := s.BindChildAuthority(t.Context(), b, a.GrantID, childTestTime); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptChild(t.Context(), req, childTestTime); !errors.Is(err, ErrChildAuthorityChanged) {
		t.Fatalf("superseded grant recovered custody: %v", err)
	}
	req.Authority = &b
	got, duplicate, err := s.AcceptChild(t.Context(), req, childTestTime)
	if err != nil || !duplicate || got.ChildID != child.ChildID {
		t.Fatalf("replacement changed child identity: %+v %t %v", got, duplicate, err)
	}
	if err := s.RevokeChildAuthority(t.Context(), a); !errors.Is(err, ErrChildAuthorityChanged) {
		t.Fatalf("old attempt revoked replacement: %v", err)
	}
	if err := s.RevokeChildAuthority(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeChildAuthority(t.Context(), b); err != nil {
		t.Fatalf("repeated revocation: %v", err)
	}
	if _, _, err := s.AcceptChild(t.Context(), req, childTestTime); !errors.Is(err, ErrChildAuthorityChanged) {
		t.Fatalf("revoked authority recovered custody: %v", err)
	}
	if err := s.BindChildAuthority(t.Context(), b, b.GrantID, childTestTime); !errors.Is(err, ErrChildAuthorityChanged) {
		t.Fatalf("revoked grant reactivated: %v", err)
	}
	c := b
	c.GrantID = "grant-c"
	if err := s.BindChildAuthority(t.Context(), c, b.GrantID, childTestTime); !errors.Is(err, ErrChildAuthorityChanged) {
		t.Fatalf("revoked attempt renewed: %v", err)
	}
	c.AttemptID = "attempt-c"
	if err := s.BindChildAuthority(t.Context(), c, b.GrantID, childTestTime); err != nil {
		t.Fatalf("replacement of revoked attempt: %v", err)
	}
}

func TestChildAuthorityChecksExpiryPolicyAndOccurrence(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	req := childRequest("parent", "stage", "call")
	a := childAuthorityFixture(req)
	if err := s.BindChildAuthority(t.Context(), a, "", childTestTime); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ChildAuthority){
		func(a *ChildAuthority) { a.Gaggle = "other" },
		func(a *ChildAuthority) { a.ParentRunID = "other" },
		func(a *ChildAuthority) { a.StageOccurrence = "other" },
		func(a *ChildAuthority) { a.GrantID = "other" },
		func(a *ChildAuthority) { a.AttemptID = "other" },
		func(a *ChildAuthority) { a.ConfigDigest = "sha256:" + strings.Repeat("c", 64) },
		func(a *ChildAuthority) { a.PolicyDigest = "sha256:" + strings.Repeat("c", 64) },
		func(a *ChildAuthority) { a.ExpiresAt = a.ExpiresAt.Add(time.Hour) },
		func(a *ChildAuthority) { a.Revoked = true },
	} {
		changed := a
		change(&changed)
		req.Authority = &changed
		if _, _, err := s.AcceptChild(t.Context(), req, childTestTime); !errors.Is(err, ErrChildAuthorityChanged) {
			t.Fatalf("changed authority admitted: %+v %v", changed, err)
		}
	}
	req.Authority = &a
	if _, _, err := s.AcceptChild(t.Context(), req, a.ExpiresAt); !errors.Is(err, ErrChildAuthorityChanged) {
		t.Fatalf("expired authority admitted: %v", err)
	}
	if count := childTableCount(t, s, "child_lineages"); count != 0 {
		t.Fatalf("refused requests changed custody: %d", count)
	}
	if err := s.FenceChildParent(t.Context(), a.ChildParent, "operator", childTestTime); err != nil {
		t.Fatal(err)
	}
	if err := s.BindChildAuthority(t.Context(), a, "", childTestTime); !errors.Is(err, ErrParentCancelled) {
		t.Fatalf("fenced parent rebound: %v", err)
	}
}

func TestChildAuthorityRevocationSerializesWithAcceptance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "starts.db")
	s, other := openTestStore(t, path), openTestStore(t, path)
	req := childRequest("parent", "stage", "call")
	a := childAuthorityFixture(req)
	req.Authority = &a
	if err := s.BindChildAuthority(t.Context(), a, "", childTestTime); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	wait.Add(2)
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		defer wait.Done()
		<-start
		_, _, err := s.AcceptChild(t.Context(), req, childTestTime)
		errs <- err
	}()
	go func() { defer wait.Done(); <-start; errs <- other.RevokeChildAuthority(t.Context(), a) }()
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, ErrChildAuthorityChanged) {
			t.Fatalf("unserialized transaction: %v", err)
		}
	}
	if _, _, err := s.AcceptChild(t.Context(), req, childTestTime); !errors.Is(err, ErrChildAuthorityChanged) {
		t.Fatalf("revocation completed but later acceptance succeeded: %v", err)
	}
}

func TestChildAuthorityRetentionIsBoundedAndPreservesActiveParent(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	req := childRequest("parent", "stage", "call")
	for i := range 105 {
		a := childAuthorityFixture(req)
		a.StageOccurrence = fmt.Sprintf("stage-%d", i)
		if err := s.BindChildAuthority(t.Context(), a, "", childTestTime); err != nil {
			t.Fatal(err)
		}
	}
	later := childTestTime.Add(365 * 24 * time.Hour)
	if result, err := s.PruneChildren(t.Context(), later, 100); err != nil || result.total() != 0 {
		t.Fatalf("active parent lost authority custody: %+v %v", result, err)
	}
	if err := s.MarkChildParentSettled(t.Context(), req.Identity.ChildParent, childTestTime); err != nil {
		t.Fatal(err)
	}
	result, err := s.PruneChildren(t.Context(), later, 100)
	if err != nil || result.AuthoritiesDeleted != 100 || result.total() != 100 {
		t.Fatalf("unbounded first sweep: %+v %v", result, err)
	}
	result, err = s.PruneChildren(t.Context(), later, 100)
	if err != nil || result.AuthoritiesDeleted != 5 || result.ParentsDeleted != 1 {
		t.Fatalf("final sweep: %+v %v", result, err)
	}
	for _, table := range []string{"child_authorities", "child_parents"} {
		if count := childTableCount(t, s, table); count != 0 {
			t.Fatalf("%s accumulated %d rows", table, count)
		}
	}
}

func TestChildAuthorityCapacityDoesNotOrphanParentOrBlockRevocation(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	a := childAuthorityFixture(childRequest("parent", "stage", "call"))
	if err := s.BindChildAuthority(t.Context(), a, "", childTestTime); err != nil {
		t.Fatal(err)
	}
	_, err := s.db.Exec(`WITH RECURSIVE ids(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM ids WHERE n<?)
 INSERT INTO child_authorities(gaggle,parent_run,occurrence,grant_id,attempt_id,config_digest,policy_digest,expires_ns)
 SELECT gaggle,parent_run,'other-'||n,grant_id,attempt_id,config_digest,policy_digest,expires_ns
 FROM child_authorities,ids WHERE occurrence='stage'`, MaxChildAuthorities-1)
	if err != nil {
		t.Fatal(err)
	}
	b := a
	b.ParentRunID = "other-parent"
	if err := s.BindChildAuthority(t.Context(), b, "", childTestTime); !errors.Is(err, ErrFull) {
		t.Fatalf("authority capacity did not backpressure: %v", err)
	}
	if count := childTableCount(t, s, "child_parents"); count != 1 {
		t.Fatalf("failed admission orphaned a parent: %d", count)
	}
	if err := s.RevokeChildAuthority(t.Context(), a); err != nil {
		t.Fatalf("full store prevented revocation: %v", err)
	}
	c := a
	c.GrantID, c.AttemptID = "replacement-grant", "replacement-attempt"
	if err := s.BindChildAuthority(t.Context(), c, a.GrantID, childTestTime); err != nil {
		t.Fatalf("full store prevented replacement: %v", err)
	}
	if count := childTableCount(t, s, "child_authorities"); count != MaxChildAuthorities {
		t.Fatalf("replacement accumulated custody: %d", count)
	}
}
