package triggerqueue

import (
	"testing"
	"time"
)

// seedLegacyChild writes the child admission layout used by schemas 5–11.
// Migration fixtures must not call today's intake implementation against an old
// schema: current capacity accounting also reads tables introduced much later.
// Keep the old database intact until Open performs the actual upgrade under test.
func seedLegacyChild(t *testing.T, s *Store, req ChildAcceptance, now time.Time) ChildRecord {
	t.Helper()
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	id := req.Identity
	runID := childDigest([]byte(id.ParentRunID + "/" + id.StageOccurrence + "/" + id.InvocationKey))[:32]
	acceptanceID := "trigger-" + runID
	startKey := childStartKey(id)
	proposalDigest := ""
	if req.Proposal != nil {
		proposalDigest = req.Proposal.Digest
		exec(`INSERT INTO child_proposals(gaggle,digest,source) VALUES(?,?,?)`, id.Gaggle, proposalDigest, req.Proposal.Source)
	}
	exec(`INSERT INTO child_parents(gaggle,parent_run,created_ns) VALUES(?,?,?)`, id.Gaggle, id.ParentRunID, now.UnixNano())
	if a := req.Authority; a != nil {
		exec(`INSERT INTO child_authorities(gaggle,parent_run,occurrence,grant_id,attempt_id,config_digest,policy_digest,expires_ns) VALUES(?,?,?,?,?,?,?,?)`, a.Gaggle, a.ParentRunID, a.StageOccurrence, a.GrantID, a.AttemptID, a.ConfigDigest, a.PolicyDigest, a.ExpiresAt.UnixNano())
	}
	exec(`INSERT INTO child_occurrences(gaggle,parent_run,occurrence,accepted_count,max_children) VALUES(?,?,?,1,?)`, id.Gaggle, id.ParentRunID, id.StageOccurrence, req.MaxChildren)
	exec(`INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES(?,?,?,?,?,?)`, acceptanceID, startKey, req.Actor, req.Payload, Accepted, now.UnixNano())
	exec(`INSERT INTO child_lineages(gaggle,parent_run,occurrence,invocation_key,sequence,child_id,acceptance_id,start_key,actor_digest,payload_digest,state,accepted_ns,updated_ns,proposal_digest,reserved_bytes) VALUES(?,?,?,?,1,?,?,?,?,?,?,?,?,?,?)`, id.Gaggle, id.ParentRunID, id.StageOccurrence, id.InvocationKey, "child-"+runID, acceptanceID, startKey, childDigest([]byte(req.Actor)), childDigest(req.Payload), ChildQueued, now.UnixNano(), now.UnixNano(), proposalDigest, childStorageReservation(req))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	child, err := s.GetChild(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return child
}

// seedLegacyDisposition builds a schema-9 returned child with its original grant.
func seedLegacyDisposition(t *testing.T, s *Store, parent string, at time.Time) (ChildRecord, ChildDispositionRequest) {
	t.Helper()
	req := childRequest(parent, "stage", "key")
	a := childAuthorityFixture(req)
	a.ExpiresAt = at.Add(time.Hour)
	req.Authority = &a
	child := seedLegacyChild(t, s, req, at)
	r := childResultValue("terminal", "")
	if err := s.KeepChildResult(t.Context(), child, r); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChildState(t.Context(), child.Identity, ChildStateUpdate{Expected: ChildQueued, State: ChildFailed, ResultRef: r.ReceiptDigest}, at); err != nil {
		t.Fatal(err)
	}
	return child, ChildDispositionRequest{Identity: child.Identity, Action: "discard", ResultRef: r.ReceiptDigest, Authority: a}
}
