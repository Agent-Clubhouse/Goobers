package triggerqueue

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestChildProposalRetentionCountsGaggleScopedOwners(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	p := &ChildProposal{Source: []byte("exact authored source")}
	p.Digest = "sha256:" + childDigest(p.Source)
	var children []ChildRecord
	for _, parent := range []string{"first", "second", "foreign"} {
		req := childRequest(parent, "stage", "call")
		req.Proposal = p
		if parent == "foreign" {
			req.Identity.Gaggle = "other"
		}
		children = append(children, acceptChildTest(t, s, req, childTestTime))
	}
	if n := childTableCount(t, s, "child_proposals"); n != 2 {
		t.Fatalf("content-addressed gaggle custody=%d, want2", n)
	}
	settle := func(c ChildRecord) {
		failChildTest(t, s, c, childTestTime, true)
		if err := s.MarkChildParentSettled(t.Context(), c.Identity.ChildParent, childTestTime); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PruneChildren(t.Context(), childTestTime.Add(ChildRetention), 100); err != nil {
			t.Fatal(err)
		}
	}
	settle(children[0])
	if n := childTableCount(t, s, "child_proposals"); n != 2 {
		t.Fatalf("first owner pruned shared artifact: %d", n)
	}
	if retained, err := s.ChildProposal(t.Context(), children[1].Identity); err != nil || string(retained.Source) != string(p.Source) {
		t.Fatalf("shared retained source=%+v, %v", retained, err)
	}
	settle(children[1])
	if n := childTableCount(t, s, "child_proposals"); n != 1 {
		t.Fatalf("last owner did not release artifact: %d", n)
	}
	if _, err := s.ChildProposal(t.Context(), children[0].Identity); !errors.Is(err, ErrChildProposalUnavailable) {
		t.Fatalf("tombstoned artifact read=%v", err)
	}
	if retained, err := s.ChildProposal(t.Context(), children[2].Identity); err != nil || retained.Digest != p.Digest {
		t.Fatalf("another gaggle's owner affected: %+v, %v", retained, err)
	}
}

func TestChildProposalBytesAndDigestAreBoundedBeforeAcceptance(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	for _, source := range [][]byte{nil, make([]byte, MaxChildProposalBytes+1)} {
		req := childRequest("parent", "stage", "call")
		req.Proposal = &ChildProposal{Source: source, Digest: "sha256:" + childDigest(source)}
		if _, _, err := s.AcceptChild(t.Context(), req, childTestTime); !errors.Is(err, ErrChildProposalUnavailable) {
			t.Fatalf("source bound=%v", err)
		}
	}
	req := childRequest("parent", "stage", "call")
	req.Proposal = &ChildProposal{Source: []byte("source"), Digest: "sha256:" + childDigest([]byte("different"))}
	if _, _, err := s.AcceptChild(t.Context(), req, childTestTime); !errors.Is(err, ErrChildProposalUnavailable) {
		t.Fatalf("digest mismatch=%v", err)
	}
	for _, table := range []string{"triggers", "child_proposals", "child_lineages"} {
		if n := childTableCount(t, s, table); n != 0 {
			t.Fatalf("invalid artifact left %s rows=%d", table, n)
		}
	}
}
