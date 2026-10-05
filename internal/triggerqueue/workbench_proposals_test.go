package triggerqueue

import (
	"crypto/sha1"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

func proposalFixture(t *testing.T, kind providers.ProviderKind, key string) (WorkbenchProposalInput, WorkbenchProposalPlan) {
	t.Helper()
	raw := []byte("# Existing plan\n\nBody.\n")
	blob := sha1.Sum(append(fmt.Appendf(nil, "blob %d\x00", len(raw)), raw...))
	target := apiv1.InteractiveRepositoryIdentity{Provider: apiv1.Provider(kind), Owner: "org", Name: "repo"}
	if kind == providers.ProviderADO {
		target.Project = "project"
	}
	source := workbench.BoundSource{Spec: apiv1.WorkbenchSource{Name: "strategy", Kind: "documents", Repository: &target, Paths: []string{"plan.md"}, Writes: &apiv1.WorkbenchWrites{Fields: []apiv1.WorkbenchField{"description"}}}, Repository: apiv1.RepoRef{Provider: target.Provider, Owner: target.Owner, Name: target.Name, Project: target.Project, Branch: "main"}}
	set := workbench.SourceSet{Scope: workbench.Scope{GaggleID: "team", Bindings: map[string]bool{"strategy": true}}, Sources: []workbench.BoundSource{source}}
	value := "# Revised plan\n\nHuman changes.\n"
	request := workbench.MetadataChangeRequest{Path: "plan.md", Expected: workbench.MetadataRevision{Commit: strings.Repeat("a", 40), BlobID: fmt.Sprintf("%x", blob), ContentDigest: childDigest(raw)}, Field: "description", Value: &value}
	file := workbench.MetadataFile{Path: request.Path, Content: raw, Provenance: workbench.SourceProvenance{Commit: request.Expected.Commit, BlobID: request.Expected.BlobID, ContentDigest: request.Expected.ContentDigest}}
	preview, err := workbench.PreviewMetadataChange(set, "strategy", file, request)
	if err != nil {
		t.Fatal(err)
	}
	scope := workbenchInput(key).Scope
	scope.SourceBindingID = "strategy"
	input := WorkbenchProposalInput{Scope: scope, RequestID: key, TargetDigest: preview.TargetDigest, OperationDigest: preview.OperationDigest, Request: request}
	plan := WorkbenchProposalPlan{Scope: set.Scope, Kind: "documents", Preview: preview, Native: providers.RepositoryProposal{Repository: providers.RepositoryRef{Provider: kind, Owner: target.Owner, Name: target.Name, Project: target.Project}, OperationDigest: preview.OperationDigest, BaseBranch: "main", BaseCommit: request.Expected.Commit, Path: request.Path, PreviousBlob: request.Expected.BlobID, Content: []byte(preview.After), Title: "Update metadata"}}
	return input, plan
}
func acceptProposal(t *testing.T, s *Store, kind providers.ProviderKind, key string) WorkbenchProposal {
	t.Helper()
	input, plan := proposalFixture(t, kind, key)
	record, dup, err := s.AcceptWorkbenchProposal(t.Context(), input, childTestTime)
	if err != nil || dup {
		t.Fatal(record, dup, err)
	}
	plan.Native.CommandID = record.ID[10:]
	marker := providers.RepositoryProposalMarker(plan.Native.CommandID, plan.Native.OperationDigest)
	plan.Native.Message = "Update metadata\n\n" + marker
	plan.Native.Body = "Human-requested metadata update.\n\n" + marker
	record, err = s.AttachWorkbenchProposalPlan(t.Context(), input.Scope, record.ID, record.RequestDigest, plan)
	if err != nil || record.State != "prepared" {
		t.Fatal(record, err)
	}
	return record
}
func claimProposal(t *testing.T, s *Store, r WorkbenchProposal) WorkbenchProposal {
	t.Helper()
	index := len(r.Phases)
	phase := proposalPhaseNames(r.Plan.Native.Repository.Provider)[index]
	got, claimed, err := s.ClaimWorkbenchProposalPhase(t.Context(), r.Input.Scope, r.ID, r.RequestDigest, phase, childTestTime.Add(time.Duration(2*index+1)*time.Second))
	if err != nil || !claimed {
		t.Fatal(got, claimed, err)
	}
	return got
}
func proposalAck(r WorkbenchProposal) providers.RepositoryProposalPhaseResult {
	phase := r.Phases[len(r.Phases)-1].Name
	result := providers.RepositoryProposalPhaseResult{MutationAttempted: true, Acknowledged: true}
	switch phase {
	case "tree":
		result.TreeID = strings.Repeat("b", 40)
	case "commit":
		result.TreeID = strings.Repeat("b", 40)
		result.CommitID = strings.Repeat("c", 40)
	case "branch":
		result.CommitID = strings.Repeat("c", 40)
		if r.Plan.Native.Repository.Provider == providers.ProviderADO {
			result.CommitID = r.Plan.Native.BaseCommit
		}
	case "pull-request":
		result.CommitID = strings.Repeat("c", 40)
		url := "https://github.com/org/repo/pull/12"
		if r.Plan.Native.Repository.Provider == providers.ProviderADO {
			url = "https://dev.azure.com/org/project/_git/repo/pullrequest/12"
		}
		result.PullRequest = &providers.PullRequestResult{ID: "12", Number: 12, URL: url}
	}
	return result
}
func completeProposal(t *testing.T, s *Store, r WorkbenchProposal, result providers.RepositoryProposalPhaseResult) WorkbenchProposal {
	t.Helper()
	index := len(r.Phases) - 1
	got, err := s.CompleteWorkbenchProposalPhase(t.Context(), r.Input.Scope, r.ID, r.RequestDigest, index, result, childTestTime.Add(time.Duration(2*index+2)*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func TestWorkbenchProposalPlanAndPhaseCustodySurviveReopen(t *testing.T) {
	for _, kind := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
		t.Run(string(kind), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "queue.db")
			s := openTestStore(t, path)
			record := acceptProposal(t, s, kind, "request")
			originalPlan := record.PlanDigest
			for record.State == "prepared" {
				record = claimProposal(t, s, record)
				record = completeProposal(t, s, record, proposalAck(record))
				if record.HistoryDigest == "" {
					t.Fatal("return omitted persisted history digest")
				}
			}
			if record.State != "confirmed" || record.CompletedAt == nil {
				t.Fatal(record)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openTestStore(t, path)
			got, err := reopened.WorkbenchProposal(t.Context(), record.Input.Scope, record.ID)
			if err != nil || got.State != "confirmed" || got.PlanDigest != originalPlan || got.Plan.Preview.After != *got.Input.Request.Value {
				t.Fatal(got, err)
			}
			if _, claimed, err := reopened.ClaimWorkbenchProposalPhase(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, "pull-request", childTestTime.Add(time.Hour)); err != nil || claimed {
				t.Fatal("replayed PR", err)
			}
			if _, err := reopened.ObserveWorkbenchProposal(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, len(record.Phases)-1, providers.RepositoryProposalObservation{}, childTestTime.Add(time.Hour)); !errors.Is(err, ErrTransition) {
				t.Fatal("terminal observation changed receipt", err)
			}
			var reserved int
			if err := reopened.db.QueryRow(`SELECT reserved_bytes FROM workbench_proposals WHERE id=?`, record.ID).Scan(&reserved); err != nil || reserved != 0 {
				t.Fatal(reserved, err)
			}
		})
	}
}
func TestWorkbenchProposalClaimRaceAndExactScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	left := openTestStore(t, path)
	right := openTestStore(t, path)
	record := acceptProposal(t, left, providers.ProviderGitHub, "key")
	var claims atomic.Int32
	var group sync.WaitGroup
	for i := range 12 {
		group.Go(func() {
			s := left
			if i%2 == 1 {
				s = right
			}
			_, claimed, err := s.ClaimWorkbenchProposalPhase(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, "tree", childTestTime.Add(time.Second))
			if err != nil {
				t.Error(err)
			}
			if claimed {
				claims.Add(1)
			}
		})
	}
	group.Wait()
	if claims.Load() != 1 {
		t.Fatal(claims.Load())
	}
	duplicate, dup, err := right.AcceptWorkbenchProposal(t.Context(), record.Input, childTestTime.Add(time.Hour))
	if err != nil || !dup || duplicate.ID != record.ID {
		t.Fatal(duplicate, dup, err)
	}
	changed := record.Input
	value := "Other body"
	changed.Request.Value = &value
	if _, _, err = right.AcceptWorkbenchProposal(t.Context(), changed, childTestTime); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	scope := record.Input.Scope
	scope.Actor.Subject = "other"
	if _, err = right.WorkbenchProposal(t.Context(), scope, record.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign actor read proposal", err)
	}
	scope = record.Input.Scope
	scope.Gaggle = "other"
	if _, err = right.WorkbenchProposal(t.Context(), scope, record.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign gaggle read proposal", err)
	}
	if err = left.Close(); err != nil {
		t.Fatal(err)
	}
	if err = right.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, path)
	got, claimed, err := reopened.ClaimWorkbenchProposalPhase(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, "tree", childTestTime.Add(time.Hour))
	if err != nil || claimed || got.State != "attempting" {
		t.Fatal(got, claimed, err)
	}
}
func TestWorkbenchProposalUnknownCannotBecomeAcknowledged(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	r := acceptProposal(t, s, providers.ProviderADO, "unknown")
	r = claimProposal(t, s, r)
	r = completeProposal(t, s, r, proposalAck(r))
	r = claimProposal(t, s, r)
	r = completeProposal(t, s, r, providers.RepositoryProposalPhaseResult{MutationAttempted: true})
	if r.State != "unknown" {
		t.Fatal(r.State)
	}
	observed := providers.RepositoryProposalObservation{Found: true, Matches: true, CommitID: strings.Repeat("c", 40), TreeID: strings.Repeat("b", 40)}
	got, err := s.ObserveWorkbenchProposal(t.Context(), r.Input.Scope, r.ID, r.RequestDigest, 1, observed, childTestTime.Add(5*time.Second))
	if err != nil || got.State != "prepared" || got.Phases[1].Outcome != "unknown" || got.Phases[1].Result.Acknowledged {
		t.Fatal(got, err)
	}
	if _, err = s.CompleteWorkbenchProposalPhase(t.Context(), r.Input.Scope, r.ID, r.RequestDigest, 1, proposalAck(r), childTestTime.Add(6*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatal("observation became acknowledged", err)
	}
	if len(got.Phases) != 2 {
		t.Fatal("observation automatically advanced")
	}
	got, claimed, err := s.ClaimWorkbenchProposalPhase(t.Context(), r.Input.Scope, r.ID, r.RequestDigest, "pull-request", childTestTime.Add(6*time.Second))
	if err != nil || !claimed {
		t.Fatal(got, claimed, err)
	}
	got, err = s.CompleteWorkbenchProposalPhase(t.Context(), r.Input.Scope, r.ID, r.RequestDigest, 2, providers.RepositoryProposalPhaseResult{MutationAttempted: true}, childTestTime.Add(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		got, err = s.ObserveWorkbenchProposal(t.Context(), r.Input.Scope, r.ID, r.RequestDigest, 2, providers.RepositoryProposalObservation{}, childTestTime.Add(time.Duration(8+i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(got.Observations) != MaxWorkbenchProposalObservations || got.OmittedObservations != 25 {
		t.Fatal(len(got.Observations), got.OmittedObservations)
	}
	if _, found := lastProposalObservation(got, 1); !found {
		t.Fatal("evicted dependent commit proof")
	}
	ack := proposalAck(got)
	final := providers.RepositoryProposalObservation{Found: true, Matches: true, CommitID: ack.CommitID, PullRequest: ack.PullRequest}
	got, err = s.ObserveWorkbenchProposal(t.Context(), r.Input.Scope, r.ID, r.RequestDigest, 2, final, childTestTime.Add(time.Minute))
	if err != nil || got.State != "observed" || got.Phases[2].Outcome != "unknown" {
		t.Fatal(got, err)
	}
}
func TestWorkbenchProposalADOUncertainBranchNeverConfersOwnership(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	r := claimProposal(t, s, acceptProposal(t, s, providers.ProviderADO, "branch"))
	r = completeProposal(t, s, r, providers.RepositoryProposalPhaseResult{MutationAttempted: true})
	result := providers.RepositoryProposalObservation{Found: true, Matches: true, CommitID: r.Plan.Native.BaseCommit}
	got, err := s.ObserveWorkbenchProposal(t.Context(), r.Input.Scope, r.ID, r.RequestDigest, 0, result, childTestTime.Add(3*time.Second))
	if err != nil || got.State != "unknown" {
		t.Fatal(got, err)
	}
	if _, claimed, err := s.ClaimWorkbenchProposalPhase(t.Context(), r.Input.Scope, r.ID, r.RequestDigest, "commit", childTestTime.Add(4*time.Second)); err != nil || claimed {
		t.Fatal("foreign matching ref adopted", err)
	}
}
func TestWorkbenchProposalRefusesTamperedRetainedContent(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	r := acceptProposal(t, s, providers.ProviderGitHub, "tamper")
	wrong := *r.Plan
	wrong.Preview.After = "foreign body"
	wrong.Native.Content = []byte(wrong.Preview.After)
	if _, err := s.AttachWorkbenchProposalPlan(t.Context(), r.Input.Scope, r.ID, r.RequestDigest, wrong); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE workbench_proposals SET after_source=? WHERE id=?`, []byte("foreign body"), r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WorkbenchProposal(t.Context(), r.Input.Scope, r.ID); err == nil {
		t.Fatal("tampered bytes admitted")
	}
}
