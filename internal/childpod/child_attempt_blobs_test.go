package childpod

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestContractBlobsGrantDeclaredInputsButNotSiblingArtifacts(t *testing.T) {
	q, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Close() }()
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	source := []byte("accepted workflow")
	sourceDigest := journal.Digest(source)
	id := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "gaggle", ParentRunID: strings.Repeat("2", 32)}, StageOccurrence: "occurrence", InvocationKey: "key"}
	child, _, err := q.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{Identity: id, Actor: "parent-stage", Payload: []byte(`{"request":{"workflow":"child","gaggle":"gaggle"}}`), MaxChildren: 2, Proposal: &triggerqueue.ChildProposal{Source: source, Digest: sourceDigest}}, now)
	if err != nil {
		t.Fatal(err)
	}
	host := ScopedBlobs{Queue: q, Identity: id}
	keep := func(data []byte) string {
		t.Helper()
		digest := journal.Digest(data)
		if err := host.Put(t.Context(), digest, data); err != nil {
			t.Fatal(err)
		}
		return digest
	}
	kit, contextDigest := keep([]byte("retained kit")), keep([]byte("declared context"))
	private := keep([]byte("sibling private artifact"))
	identity := journal.RunIdentity{Schema: journal.RunSchema, InstanceID: "instance", RunID: child.RunID, Workflow: "generated", Gaggle: id.Gaggle, ConfigGeneration: sourceDigest, WorkflowDigest: sourceDigest, GooberDigest: sourceDigest, StartedAt: now}
	identity.Child = &journal.ChildLineage{Gaggle: id.Gaggle, ParentRunID: id.ParentRunID, ParentWorkflow: "parent", StageOccurrence: id.StageOccurrence, InvocationKey: id.InvocationKey, AcceptanceID: child.AcceptanceID, SourceDigest: sourceDigest, EnvelopeDigest: sourceDigest}
	contract := Contract{Version: 1, Identity: identity, Stage: "stage", Attempt: 1, PodAttempt: 11, StartedAt: now, Ceiling: credentials.NewChildCeiling(false, nil, nil), KitDigest: kit, ContextDigests: []string{contextDigest}}
	raw, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	digest := keep(raw)
	attempt := ChildAttemptBlobs{Store: host, ContractDigest: digest}
	if _, err := attempt.Get(t.Context(), digest); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatal("unbound contract readable", err)
	}
	if err := host.BindContract(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{digest, kit, contextDigest} {
		if _, err := attempt.Get(t.Context(), input); err != nil {
			t.Fatal("declared input unavailable", input, err)
		}
	}
	if _, err := attempt.Get(t.Context(), private); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatal("sibling artifact exposed", err)
	}
	if err := attempt.Put(t.Context(), private, []byte("different bytes")); err == nil {
		t.Fatal("digest alone granted ownership")
	}
	if _, err := attempt.Get(t.Context(), private); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatal("failed upload granted ownership", err)
	}
	output := []byte("own output")
	outputDigest := journal.Digest(output)
	if err := attempt.Put(t.Context(), outputDigest, output); err != nil {
		t.Fatal(err)
	}
	if _, err := attempt.GetBounded(t.Context(), outputDigest, int64(len(output)-1)); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatal("byte limit ignored", err)
	}
	contract.PodAttempt++
	raw, err = json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	siblingDigest := keep(raw)
	if err := host.BindContract(t.Context(), siblingDigest); err != nil {
		t.Fatal(err)
	}
	sibling := ChildAttemptBlobs{Store: host, ContractDigest: siblingDigest}
	if _, err := sibling.Get(t.Context(), outputDigest); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatal("retry inherited previous output", err)
	}
	contract.Identity.Child.SourceDigest = journal.Digest([]byte("substituted source"))
	raw, err = json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	foreign := keep(raw)
	if err := host.BindContract(t.Context(), foreign); err == nil {
		t.Fatal("unaccepted source bound")
	}
}
