//go:build integration

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
)

func TestIntegrationHumanChildEpochPublishesFirstBranchWithInteractiveCredential(t *testing.T) {
	testProductionChildPublication(t, false, true)
}

// Common human admission/target eligibility has separate composed tests. This
// fixture starts at its durable queue+journal boundary and exercises the actual
// retained fork, credential broker and host publication factories end to end.
func prepareHumanPublicationEpoch(t *testing.T, f *childKitFixture, sourcePath string, project apiv1.RepoRef) string {
	t.Helper()
	s := f.writer.service
	source := f.writer.identity
	recorder := f.writer.recorder.(*journal.Run)
	if err := recorder.Append(journal.Event{Type: journal.EventRunFinished, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(recorder.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	terminal := events[len(events)-1]
	url, err := childRepoCloneURL(project)
	if err != nil {
		t.Fatal(err)
	}
	custody := childworkflow.WorkspaceCoordinator{Queue: s.childQueue, Worktrees: f.manager}
	sealed, err := custody.CaptureResult(t.Context(), f.child, &childworkflow.YieldedWorkspace{Path: sourcePath, RepoURL: url, RepositoryKey: childRepoKey(project), Policy: recovery.SnapshotPolicy{ExcludedPaths: []string{"private.txt"}}}, childworkflow.TerminalResultInput{State: triggerqueue.ChildFailed, FinishedAt: terminal.Time, Summary: "needs human repair"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.childQueue.SetChildState(t.Context(), f.child.Identity, triggerqueue.ChildStateUpdate{Expected: f.child.State, State: triggerqueue.ChildFailed, ResultRef: sealed.ResultRef, WorkspaceRef: sealed.WorkspaceRef}, terminal.Time); err != nil {
		t.Fatal(err)
	}
	principal := httpapi.Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []httpapi.Role{httpapi.RoleOperate}}
	authority, err := interactiveaccess.NewRestartAuthority(principal, source, strings.Repeat("e", 32), "push", terminal.Seq)
	if err != nil {
		t.Fatal(err)
	}
	authorityRaw, err := authority.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	planRaw := []byte("retained fixture admission; common plan validation tested separately")
	epoch, _, err := s.childQueue.BeginChildRestart(t.Context(), triggerqueue.ChildRestartRequest{Identity: f.child.Identity, RunID: authority.EpochID, SourceRunID: source.RunID, SourceTerminalSeq: terminal.Seq, SourceResultRef: sealed.ResultRef, Actor: principal.Issuer + ":" + principal.Subject, Stage: authority.Stage, Plan: planRaw, PlanDigest: journal.Digest(planRaw)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.child, err = s.childQueue.GetChild(t.Context(), f.child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := custody.PrepareExecution(t.Context(), f.child, url)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := f.manager.AdoptChildFromSnapshot(t.Context(), worktree.ChildOptions{RepoURL: url, RunID: admission.WorkspaceID, OwnerRunID: epoch.RunID, Gaggle: source.Gaggle, SnapshotSHA: admission.ForkSHA})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(owned.Path, "human.txt"), []byte("human epoch output"), 0600); err != nil {
		t.Fatal(err)
	}
	lineage := *source.Child
	lineage.ExecutionEpoch, lineage.PriorResultRef, lineage.RestartDigest = epoch.Epoch, epoch.SourceResultRef, epoch.RequestDigest
	inputs := map[string][]byte{interactiveaccess.RestartAuthorityInputName: authorityRaw}
	grades := map[string]apiv1.Integrity{interactiveaccess.RestartAuthorityInputName: apiv1.IntegrityTrusted}
	for _, input := range source.Inputs {
		inputs[input.Name], err = reader.ArtifactBytesBounded(input.Ref, 4<<20)
		if err != nil {
			t.Fatal(err)
		}
		grades[input.Name] = input.Integrity
	}
	plan, err := runner.BindChildRestartWorkspace(runner.StageRestartPlan{Source: source, Continuation: journal.ContinuationRequest{RunID: epoch.RunID, SourceRunID: source.RunID, ExpectedTerminalSeq: terminal.Seq, Operator: epoch.Actor, Target: epoch.Stage, ChildContinuation: &lineage, Inputs: inputs, InputIntegrity: grades, InputSource: map[string]string{interactiveaccess.RestartAuthorityInputName: epoch.Actor}}}, admission)
	if err != nil {
		t.Fatal(err)
	}
	next, err := journal.CreateContinuation(s.layout.ForGaggle(source.Gaggle).RunsDir(), plan.Continuation)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	reader, err = journal.OpenReadOnly(next.Dir())
	if err != nil {
		t.Fatal(err)
	}
	f.writer.identity, err = reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	f.writer.recorder = next
	f.attempt.Envelope.RunID = epoch.RunID
	configureHumanPublicationPolicy(t, s, f.parent.applied.Gaggles[0], principal)
	return owned.Path
}

func configureHumanPublicationPolicy(t *testing.T, s *daemonCredentialService, original apiv1.Gaggle, p httpapi.Principal) {
	t.Helper()
	t.Setenv("HUMAN_PUBLICATION_TOKEN", "human-publication-token")
	g := *original.DeepCopy()
	repo := interactiveRepository(g.Spec.Project)
	g.Spec.InteractiveAccess = &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Operators: []apiv1.InteractiveHumanGrant{{Issuer: p.Issuer, Subject: p.Subject}}}, Actions: []apiv1.InteractiveAction{"run.restartStage", "repository.read", "pr.repair"}, Credentials: apiv1.InteractiveCredentialBindings{Repositories: []apiv1.InteractiveRepositoryCredential{{Repository: repo, CredentialRef: "human-code"}}}}
	var err error
	s.interactive, err = interactiveaccess.New([]apiv1.Gaggle{g}, []instance.InteractiveCredential{{Name: "human-code", Provider: string(repo.Provider), Owner: repo.Owner, Project: repo.Project, Repository: repo.Name, Token: instance.TokenRef{Env: "HUMAN_PUBLICATION_TOKEN"}}}, interactiveaccess.Dependencies{Registrar: s.shared})
	if err != nil {
		t.Fatal(err)
	}
}
