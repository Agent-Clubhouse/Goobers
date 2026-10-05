package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func humanChildCredentialFixture(t *testing.T) (*daemonCredentialService, pinnedStage, apiv1.Gaggle) {
	t.Helper()
	t.Setenv("HUMAN_CHILD_MODEL", "sk-ant-api-human-child-model")
	t.Setenv("HUMAN_CHILD_CODE", "human-child-code-token")
	f := actualChildLaunchFixture(t)
	source := publishInterruptedChild(t, f)
	dir, err := f.launcher.layout.FindRunDir(source.RunID)
	if err != nil {
		t.Fatal(err)
	}
	writer, _, err := journal.TryRecover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.Append(journal.Event{Type: journal.EventRunFinished, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	actor := httpapi.Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []httpapi.Role{httpapi.RoleOperate}}
	authority, err := interactiveaccess.NewRestartAuthority(actor, source, strings.Repeat("c", 32), "work", events[len(events)-1].Seq)
	if err != nil {
		t.Fatal(err)
	}
	authorityRaw, err := authority.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	child, err := f.service.queue.GetChild(t.Context(), f.submission.Child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	result := triggerqueue.ChildResult{Receipt: []byte("verified stopped result")}
	result.ReceiptDigest = journal.Digest(result.Receipt)
	if err = f.service.queue.KeepChildResult(t.Context(), child, result); err != nil {
		t.Fatal(err)
	}
	if err = f.service.queue.SetChildState(t.Context(), child.Identity, triggerqueue.ChildStateUpdate{Expected: child.State, State: triggerqueue.ChildFailed, ResultRef: result.ReceiptDigest}, time.Now()); err != nil {
		t.Fatal(err)
	}
	plan := []byte("retained human stage restart")
	epoch, _, err := f.service.queue.BeginChildRestart(t.Context(), triggerqueue.ChildRestartRequest{Identity: child.Identity, RunID: authority.EpochID, SourceRunID: source.RunID, SourceTerminalSeq: authority.SourceTerminalSeq, SourceResultRef: result.ReceiptDigest, Actor: actor.Issuer + ":" + actor.Subject, Stage: authority.Stage, Plan: plan, PlanDigest: journal.Digest(plan)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	lineage := *source.Child
	lineage.ExecutionEpoch, lineage.PriorResultRef, lineage.RestartDigest = epoch.Epoch, epoch.SourceResultRef, epoch.RequestDigest
	inputs := map[string][]byte{interactiveaccess.RestartAuthorityInputName: authorityRaw}
	integrity := map[string]apiv1.Integrity{interactiveaccess.RestartAuthorityInputName: apiv1.IntegrityTrusted}
	for _, input := range source.Inputs {
		if input.Name != "child-credential-ceiling" {
			continue
		}
		inputs[input.Name], err = reader.ArtifactBytesBounded(input.Ref, 32<<10)
		if err != nil {
			t.Fatal(err)
		}
		integrity[input.Name] = input.Integrity
	}
	next, err := journal.CreateContinuation(f.launcher.layout.ForGaggle(source.Gaggle).RunsDir(), journal.ContinuationRequest{RunID: epoch.RunID, SourceRunID: source.RunID, ExpectedTerminalSeq: authority.SourceTerminalSeq, Operator: epoch.Actor, Target: epoch.Stage, ChildContinuation: &lineage, Inputs: inputs, InputIntegrity: integrity, InputSource: map[string]string{interactiveaccess.RestartAuthorityInputName: epoch.Actor}})
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err = journal.OpenReadOnly(filepath.Join(f.launcher.layout.ForGaggle(source.Gaggle).RunsDir(), epoch.RunID))
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	gaggle := *f.authority.authority.Admission.Gaggle.DeepCopy()
	// The retained queue fixture uses scratch tasks; supply an exact repository
	// scope for exercising the independently selected host publication binding.
	gaggle.Spec.Project = apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	repo := interactiveRepository(gaggle.Spec.Project)
	gaggle.Spec.InteractiveAccess = &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Operators: []apiv1.InteractiveHumanGrant{{Issuer: actor.Issuer, Subject: actor.Subject}}}, Actions: []apiv1.InteractiveAction{"run.restartStage", "repository.read", "pr.repair"}, Credentials: apiv1.InteractiveCredentialBindings{Repositories: []apiv1.InteractiveRepositoryCredential{{Repository: repo, CredentialRef: "human-code"}}}}
	service := &daemonCredentialService{layout: f.launcher.layout, childQueue: f.service.queue, childCredentials: f.launcher.credentialCeiling, shared: journal.NewRegistryScrubber(), config: &instance.Config{Credentials: []instance.CredentialGrant{{Capability: "agent:model", Harness: string(apiv1.HarnessClaudeCode), Token: instance.TokenRef{Env: "HUMAN_CHILD_MODEL"}}}}}
	service.interactive, err = interactiveaccess.New([]apiv1.Gaggle{gaggle}, []instance.InteractiveCredential{{Name: "human-code", Provider: string(repo.Provider), Owner: repo.Owner, Project: repo.Project, Repository: repo.Name, Token: instance.TokenRef{Env: "HUMAN_CHILD_CODE"}}}, interactiveaccess.Dependencies{Registrar: service.shared})
	if err != nil {
		t.Fatal(err)
	}
	service.buildSources = func(credentialGaggleScope) (credentials.Resolver, []credentials.Grant, error) {
		t.Error("human epoch reached automation credential source")
		return nil, nil, errors.New("automation forbidden")
	}
	pinned := pinnedStage{identity: id, defs: credentialPlaneDefinitions{Scopes: map[string]credentialGaggleScope{id.Gaggle: {Project: gaggle.Spec.Project, Backlog: gaggle.Spec.Backlog, AdditionalRepos: gaggle.Spec.AdditionalRepos}}, Goobers: map[string]apiv1.GooberSpec{"coder": {Harness: apiv1.HarnessClaudeCode}}}, profile: stageProfile{goober: "coder", harness: string(apiv1.HarnessClaudeCode), capabilities: []string{"agent:model", "repo:push", "provider:pr:write"}, implicitKeys: []string{"repo:push", "mcp:vendor"}}}
	return service, pinned, gaggle
}

func TestHumanChildCredentialBrokerUsesInteractiveIdentityAndFencesReload(t *testing.T) {
	service, pinned, gaggle := humanChildCredentialFixture(t)
	ctx, closeHuman, err := service.beginInteractiveChildCredentials(t.Context(), pinned)
	if err != nil {
		t.Fatal(err)
	}
	defer closeHuman()
	ctx, ceiling, err := service.applyChildCredentialCeiling(ctx, pinned)
	if err != nil {
		t.Fatal(err)
	}
	closeCeiling := sync.OnceFunc(ceiling.release)
	defer closeCeiling()
	resolved, err := service.mintStageCredentials(ctx, pinned, pinned.profile.capabilities)
	if err != nil || len(resolved.minted) != 1 || resolved.minted[0].Capability != "agent:model" || resolved.minted[0].Value != "sk-ant-api-human-child-model" {
		t.Fatal("model-only broker failed", err)
	}
	if err = ceiling.finish(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(service.shared.Scrub([]byte(resolved.minted[0].Value))), resolved.minted[0].Value) {
		t.Fatal("model credential not scrubbed")
	}
	t.Setenv("HUMAN_CHILD_MODEL", "ghp-provider-token")
	if _, err = service.mintStageCredentials(ctx, pinned, []string{"agent:model"}); err == nil {
		t.Fatal("provider token accepted as model identity")
	}
	// A host publication has the full already-verified delegated ceiling. Its
	// provider key comes only from the explicit interactive binding.
	host, err := credentials.WithChildCeiling(ctx, credentials.NewChildCeiling(true, []string{"repo:push"}, []string{"repo:push"}))
	if err != nil {
		t.Fatal(err)
	}
	hostProfile := pinned
	hostProfile.profile.implicitKeys = nil
	resolved, err = service.mintStageCredentials(host, hostProfile, []string{"repo:push"})
	if err != nil || len(resolved.minted) != 1 || resolved.minted[0].Value != "human-child-code-token" || resolved.scheme != "bearer" {
		t.Fatal("human publication identity failed", err)
	}
	gaggle.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read"}
	done := make(chan error, 1)
	go func() { done <- service.interactive.Apply([]apiv1.Gaggle{gaggle}, nil) }()
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("policy reload did not cancel credential operation")
	}
	if _, err = service.mintStageCredentials(ctx, pinned, []string{"agent:model"}); err == nil {
		t.Fatal("revoked lease minted credentials")
	}
	closeCeiling()
	closeHuman()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("credential operation deadlocked policy reload")
	}
	if _, closeAgain, err := service.beginInteractiveChildCredentials(context.Background(), pinned); err == nil {
		closeAgain()
		t.Fatal("revoked restart remained authorized")
	}
}
