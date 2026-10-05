package triggerqueue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workbench"
)

func suggestionInput(t *testing.T, decision string) WorkbenchSuggestionInput {
	t.Helper()
	target := apiv1.InteractiveRepositoryIdentity{Provider: "github", Owner: "org", Name: "repo"}
	source := workbench.BoundSource{Spec: apiv1.WorkbenchSource{Name: "strategy", Kind: "documents", Repository: &target, Paths: []string{"plan.md"}}, Repository: apiv1.RepoRef{Provider: "github", Owner: "org", Name: "repo", Branch: "main"}}
	set := workbench.SourceSet{Scope: workbench.Scope{GaggleID: "team", Bindings: map[string]bool{"strategy": true}}, Sources: []workbench.BoundSource{source}}
	digest, err := workbench.SourceTargetDigest(set.Scope, source)
	if err != nil {
		t.Fatal(err)
	}
	pins := workbench.SuggestionRepositoryRevision{Commit: strings.Repeat("a", 40), BlobID: strings.Repeat("b", 40), ContentDigest: strings.Repeat("c", 64)}
	evidence := workbench.SuggestionEvidence{SourceTargetDigest: digest, Path: "plan.md", RepositoryRevision: &pins}
	from := workbench.NodeRef{GaggleID: "team", SourceBindingID: "strategy", Kind: "objective-document", SourceID: "obj-11111111-1111-4111-8111-111111111111"}
	to := from
	to.SourceID = "obj-22222222-2222-4222-8222-222222222222"
	value := workbench.RelationshipSuggestion{Kind: "references", Rationale: "Related plan.", From: workbench.SuggestionEndpoint{Ref: &from, Evidence: &evidence}, To: workbench.SuggestionEndpoint{Ref: &to, Evidence: &evidence}}
	raw, err := json.Marshal(workbench.RelationshipSuggestions{SchemaVersion: workbench.SuggestionSchemaVersion, Suggestions: []workbench.RelationshipSuggestion{value}})
	if err != nil {
		t.Fatal(err)
	}
	path, err := journal.ArtifactPath(journal.Digest(raw))
	if err != nil {
		t.Fatal(err)
	}
	origin := workbench.SuggestionOrigin{RunID: strings.Repeat("a", 32), StageID: "curate", Attempt: 1, ArtifactDigest: childDigest(raw), ArtifactPath: path}
	bound, err := workbench.BindSuggestions(raw, set, origin)
	if err != nil {
		t.Fatal(err)
	}
	scope := workbenchInput("review").Scope
	in := WorkbenchSuggestionInput{Scope: WorkbenchSuggestionScope{Gaggle: scope.Gaggle, Actor: scope.Actor}, Suggestion: bound[0], ConfigGeneration: strings.Repeat("f", 64), StageSequence: 2, ArtifactSequence: 3, Decision: decision}
	if decision == "accept" {
		edge, _, err := workbench.MaterializeSuggestion(set, bound[0], nil)
		if err != nil {
			t.Fatal(err)
		}
		scope.SourceBindingID = "strategy"
		in.Proposal = &WorkbenchProposalInput{Scope: scope, RequestID: SuggestionProposalKey(bound[0].Key), TargetDigest: digest, OperationDigest: strings.Repeat("d", 64), Request: workbench.MetadataChangeRequest{Path: "plan.md", Expected: workbench.MetadataRevision(pins), Relationship: &workbench.MetadataRelationshipEdit{Action: "add", Edge: edge}}}
	}
	return in
}

func TestWorkbenchSuggestionExactDecisionReplayAndActorCustody(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "review.db"))
	in := suggestionInput(t, "reject")
	r, dup, err := s.AcceptWorkbenchSuggestion(t.Context(), in, childTestTime)
	if err != nil || dup || r.State != "rejected" {
		t.Fatal(r, dup, err)
	}
	in.Suggestion.Origin.RunID = strings.Repeat("b", 32)
	in.Suggestion.Proposal.Rationale = "Repeated from another actual producer."
	got, dup, err := s.AcceptWorkbenchSuggestion(t.Context(), in, childTestTime.Add(time.Hour))
	if err != nil || !dup || got.ID != r.ID || got.Input.Suggestion.Origin != r.Input.Suggestion.Origin {
		t.Fatal(got, dup, err)
	}
	in.Decision = "accept"
	in.Proposal = suggestionInput(t, "accept").Proposal
	in.Suggestion.Proposal.Rationale = in.Proposal.Request.Relationship.Edge.Rationale
	if _, _, err = s.AcceptWorkbenchSuggestion(t.Context(), in, childTestTime); !errors.Is(err, ErrConflict) {
		t.Fatal("decision replaced", err)
	}
	scope := in.Scope
	scope.Actor.Subject = "other"
	if _, err = s.WorkbenchSuggestion(t.Context(), scope, r.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign receipt", err)
	}
	if _, err = s.FindWorkbenchSuggestion(t.Context(), scope, r.Key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign key", err)
	}
	if _, err = s.db.Exec(`UPDATE workbench_suggestions SET input=replace(input,'curate','tamper') WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.WorkbenchSuggestion(t.Context(), in.Scope, r.ID); !errors.Is(err, ErrTransition) {
		t.Fatal("tampered custody", err)
	}
}

func TestWorkbenchSuggestionConcurrentAdmissionLinksOneActualProposalAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review.db")
	a, b := openTestStore(t, path), openTestStore(t, path)
	in := suggestionInput(t, "accept")
	var wg sync.WaitGroup
	ids := make(chan string, 2)
	for _, s := range []*Store{a, b} {
		wg.Go(func() {
			r, _, err := s.AcceptWorkbenchSuggestion(t.Context(), in, childTestTime)
			if err != nil {
				t.Error(err)
				return
			}
			p, _, err := s.AcceptWorkbenchProposal(t.Context(), *in.Proposal, childTestTime)
			if err != nil {
				t.Error(err)
				return
			}
			linked, err := s.LinkWorkbenchSuggestion(t.Context(), in.Scope, r.ID, p.ID, childTestTime.Add(time.Second))
			if err != nil {
				t.Error(err)
				return
			}
			ids <- linked.ProposalID
		})
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first != "" && first != id {
			t.Fatal("multiple proposals", first, id)
		}
		first = id
	}
	if first == "" {
		t.Fatal("no proposal")
	}
	c := openTestStore(t, path)
	r, err := c.FindWorkbenchSuggestion(t.Context(), in.Scope, in.Suggestion.Key)
	if err != nil || r.State != "linked" || r.ProposalID != first {
		t.Fatal(r, err)
	}
	wrong := *in.Proposal
	wrong.RequestID = "different-command"
	p, _, err := c.AcceptWorkbenchProposal(t.Context(), wrong, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.LinkWorkbenchSuggestion(t.Context(), in.Scope, r.ID, p.ID, childTestTime); !errors.Is(err, ErrConflict) {
		t.Fatal("replaced linked intent", err)
	}
}

func TestWorkbenchSuggestionProductionMaintenancePinsPendingAndProposal(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "review.db"))
	in := suggestionInput(t, "accept")
	r, _, err := s.AcceptWorkbenchSuggestion(t.Context(), in, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	later := childTestTime.Add(100 * 24 * time.Hour)
	if n, err := s.PruneWorkbenchCommands(t.Context(), later, 100); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	p, _, err := s.AcceptWorkbenchProposal(t.Context(), *in.Proposal, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.LinkWorkbenchSuggestion(t.Context(), in.Scope, r.ID, p.ID, childTestTime); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneWorkbenchCommands(t.Context(), later, 100); err != nil || n != 0 {
		t.Fatal("pending linked intent pruned", n, err)
	}
	if retained, err := s.SuggestionRunRetained(t.Context(), in.Scope.Gaggle, in.Suggestion.Origin.RunID); err != nil || !retained {
		t.Fatal(retained, err)
	}
	if pins, err := s.RetainedSuggestionGenerations(t.Context()); err != nil || len(pins) != 1 || pins[0] != in.ConfigGeneration {
		t.Fatal(pins, err)
	}
	if _, err = s.StopWorkbenchProposal(t.Context(), p.Input.Scope, p.ID, p.RequestDigest, childTestTime); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneWorkbenchProposals(t.Context(), later, 100); err != nil || n != 0 {
		t.Fatal("linked evidence erased first", n, err)
	}
	if n, err := s.PruneWorkbenchCommands(t.Context(), later, 100); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if retained, err := s.SuggestionRunRetained(t.Context(), in.Scope.Gaggle, in.Suggestion.Origin.RunID); err != nil || retained {
		t.Fatal(retained, err)
	}
	if _, _, err = s.AcceptWorkbenchSuggestion(t.Context(), in, later); !errors.Is(err, ErrWorkbenchCommandExpired) {
		t.Fatal("tombstone accepted", err)
	}
	if n, err := s.PruneWorkbenchCommands(t.Context(), later, 100); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if n, err := s.PruneWorkbenchCommands(t.Context(), later.Add(WorkbenchCommandRetention), 100); err != nil || n != 2 {
		t.Fatal(n, err)
	}
}

func TestWorkbenchSuggestionSharedQuotaAndDefensiveBounds(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "review.db"))
	in := suggestionInput(t, "reject")
	r, _, err := s.AcceptWorkbenchSuggestion(t.Context(), in, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < MaxWorkbenchCommands; i++ {
		_, err = s.db.Exec(`INSERT INTO workbench_suggestions(id,key_digest,gaggle,issuer,subject,suggestion_key,input_digest,input,state,accepted_ns,origin_run,config_generation) SELECT ?,?,gaggle,issuer,subject,suggestion_key,input_digest,input,state,accepted_ns,origin_run,config_generation FROM workbench_suggestions WHERE id=?`, fmt.Sprintf("quota-%d", i), fmt.Sprintf("key-%d", i), r.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err = s.AcceptWorkbenchCommand(t.Context(), workbenchInput("native"), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("native ignored shared quota", err)
	}
	if _, err = s.db.Exec(`DELETE FROM workbench_suggestions WHERE id<>?`, r.ID); err != nil {
		t.Fatal(err)
	}
	in.Scope.Actor.Subject = "other"
	if _, err = s.db.Exec(`UPDATE workbench_suggestions SET reserved_bytes=? WHERE id=?`, childStoreByteCeiling, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.AcceptWorkbenchSuggestion(t.Context(), in, childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("byte quota ignored", err)
	}
	in.Reason = strings.Repeat("a", 4097)
	if _, _, err = s.AcceptWorkbenchSuggestion(t.Context(), in, childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatal("unbounded reason", err)
	}
}
