package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/childpublication"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/providers"
)

func TestReadChildPublicationsPreservesUncertainCustody(t *testing.T) {
	f := newHandoffDaemonFixture(t)
	lineage := journal.ChildLineage{Gaggle: f.child.Identity.Gaggle, ParentRunID: f.child.Identity.ParentRunID, StageOccurrence: f.child.Identity.StageOccurrence, InvocationKey: f.child.Identity.InvocationKey, AcceptanceID: f.child.AcceptanceID, SourceDigest: f.child.ProposalDigest, EnvelopeDigest: journal.Digest([]byte("envelope"))}
	intent := childpublication.BranchIntent{Version: 1, RunID: f.child.RunID, Lineage: lineage, Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}, Remote: "https://github.com/owner/repo.git", Head: "factory/children/" + f.child.RunID, Base: "main", Commit: strings.Repeat("a", 40)}
	raw, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := f.queue.PrepareChildExecutionPublication(t.Context(), f.child.Identity, f.child.RunID, "branch", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.queue.BeginChildExecutionPublicationEffect(t.Context(), publication, f.child.RunID); err != nil {
		t.Fatal(err)
	}
	before, err := f.queue.ChildPublication(t.Context(), f.child.Identity, "branch")
	if err != nil {
		t.Fatal(err)
	}
	session := &upSession{}
	session.durableTriggers = &durableTriggerService{queue: f.queue}
	statuses, err := session.readChildPublications(t.Context(), f.child.Identity)
	if err != nil || len(statuses) != 1 {
		t.Fatal(statuses, err)
	}
	expected := readservice.ChildPublicationObservation{SourceRunID: f.child.RunID, Item: readservice.ChildPublicationItem{Action: "branch", State: "effect_pending", Head: intent.Head, Base: "main", Commit: intent.Commit, NeedsHuman: true}}
	if !reflect.DeepEqual(statuses[0], expected) {
		t.Fatalf("unsafe or missing publication projection: %#v", statuses[0])
	}
	after, err := f.queue.ChildPublication(t.Context(), f.child.Identity, "branch")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("read mutated publication custody", err)
	}
	child, err := f.queue.GetChild(t.Context(), f.child.Identity)
	if err != nil || !reflect.DeepEqual(child, f.child) {
		t.Fatal("read mutated child acceptance", err)
	}
	empty := &upSession{}
	if _, err := empty.readChildPublications(t.Context(), f.child.Identity); !errors.Is(err, readservice.ErrChildHistoryUnavailable) {
		t.Fatal(err)
	}
}
