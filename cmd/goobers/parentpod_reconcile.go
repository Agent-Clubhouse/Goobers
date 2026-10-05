package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

func (s *daemonCredentialService) installParentRecovery(registry *daemonRunnerRegistry) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.reconcileContained = func(ctx context.Context, id journal.RunIdentity) error {
		if s.parentRecovery == nil {
			return nil
		}
		dir, err := s.layout.FindRunDir(id.RunID)
		if err != nil {
			return err
		}
		reader, err := journal.OpenReadOnly(dir)
		if err != nil {
			return err
		}
		pending, _, err := pendingParentPodScopes(reader)
		if err != nil || len(pending) == 0 {
			return err
		}
		release, ok := registry.acquireChildCustody(id.RunID)
		if !ok {
			return journal.ErrRecoveryBusy
		}
		defer release()
		return s.parentRecovery(ctx, id)
	}
}

// The registry reservation excludes all Runner owners. Read-only selection
// precedes worker teardown; only verified stopped custody opens a journal writer.
func (p *parentStagePod) reconcile(ctx context.Context) error {
	dir, err := p.service.layout.FindRunDir(p.identity.RunID)
	if err != nil {
		return err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return err
	}
	actual, err := reader.Identity()
	if err != nil || !reflect.DeepEqual(actual, p.identity) {
		return errors.Join(errors.New("parent recovery identity changed"), err)
	}
	pending, events, err := pendingParentPodScopes(reader)
	if err != nil || len(pending) == 0 {
		return err
	}
	digests, err := orderedParentRecoveries(pending, events)
	if err != nil {
		return err
	}
	for _, digest := range digests {
		scope := pending[digest]
		allowed, err := parentPodCustodyPending(reader, digest)
		if err != nil || !allowed {
			return errors.Join(invoke.ErrWorkspaceNotQuiescent, err)
		}
		retained, err := retainedParentAttempt(reader, scope, digest)
		if err != nil {
			return err
		}
		workspace, err := p.recoveryWorkspace(ctx, events, scope)
		if err != nil {
			return err
		}
		transport := childpod.TemporalDispatch{Client: p.client}
		report, dispatchErr := transport.Reconcile(ctx, retained.Input)
		if !report.WorkspaceWritersStopped || !report.SurrenderConfirmed {
			return errors.Join(invoke.ErrWorkspaceNotQuiescent, dispatchErr)
		}
		writer, _, err := journal.TryRecover(dir, journal.WithScrubber(p.service.shared))
		if err != nil {
			return err
		}
		err = p.importRecovery(ctx, reader, writer, scope, retained, digest, workspace, report)
		closeErr := writer.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return err
		}
	}
	return nil
}

func orderedParentRecoveries(pending map[string]parentPodScope, events []journal.Event) ([]string, error) {
	if len(pending) > 128 {
		return nil, errors.New("parent recovery exceeds branch bound")
	}
	digests := make([]string, 0, len(pending))
	workspaces := map[string]bool{}
	for digest, scope := range pending {
		custody, err := parentHeldReceipt(events, scope)
		if err != nil {
			return nil, err
		}
		key := custody.Workspace.RepositoryDigest + "/" + custody.Workspace.WorkspaceID
		if workspaces[key] {
			return nil, errors.New("parallel parent scopes share workspace custody")
		}
		workspaces[key] = true
		digests = append(digests, digest)
	}
	slices.SortFunc(digests, func(a, b string) int {
		return pending[a].Contract.PodAttempt - pending[b].Contract.PodAttempt
	})
	return digests, nil
}

func retainedParentAttempt(reader *journal.Reader, scope parentPodScope, digest string) (childpod.RetainedAttempt, error) {
	data, err := json.Marshal(scope.Event.Runner["retainedAttempt"])
	if err != nil {
		return childpod.RetainedAttempt{}, err
	}
	var ref journal.Ref
	if json.Unmarshal(data, &ref) != nil || ref.Path == "" {
		return childpod.RetainedAttempt{}, errors.New("parent exact retained attempt is missing")
	}
	retained, err := childpod.ReadRetainedAttempt(reader, ref)
	if err != nil {
		return retained, err
	}
	a, c := retained.Input.Attempt, scope.Contract
	if a.ChildExecutionDigest != digest || a.RunID != c.Identity.RunID || a.InstanceID != c.Identity.InstanceID || a.Gaggle != c.Identity.Gaggle || a.Workflow != c.Identity.Workflow || a.Stage != c.Stage || a.Number != c.Attempt || a.PodAttempt != c.PodAttempt || !a.WorkflowParent || a.Envelope == nil || !reflect.DeepEqual(a.Envelope.ChildWorkflowOrigin, c.ParentOrigin) {
		return retained, errors.New("parent retained dispatch differs from physical contract")
	}
	return retained, nil
}

func (p *parentStagePod) recoveryWorkspace(ctx context.Context, events []journal.Event, scope parentPodScope) (*worktree.Worktree, error) {
	custody, err := parentHeldReceipt(events, scope)
	if err != nil {
		return nil, err
	}
	store, err := executionGenerationStore(p.service.layout)
	if err != nil {
		return nil, err
	}
	directory, lease, err := store.Acquire(ctx, p.identity.ConfigGeneration)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lease.Release() }()
	set, _, err := loadConfigDirectory(directory)
	if err != nil {
		return nil, err
	}
	for i := range set.Gaggles {
		gaggle := &set.Gaggles[i]
		if gaggle.Name != p.identity.Gaggle {
			continue
		}
		url, err := repoCloneURL(gaggle.Spec.Project)
		if err != nil {
			return nil, err
		}
		layout, err := instance.EffectiveWorkcopiesLayout(p.service.layout.ForGaggle(p.identity.Gaggle), p.service.config, gaggle)
		if err != nil {
			return nil, err
		}
		manager, err := worktree.NewManager(layout.WorkcopiesDir())
		if err != nil {
			return nil, err
		}
		return manager.AdoptHeldStage(ctx, url, custody.Workspace)
	}
	return nil, errors.New("retained parent gaggle is missing")
}

func parentHeldReceipt(events []journal.Event, scope parentPodScope) (runner.ContainedParentWorkspaceCustody, error) {
	var receipt runner.ContainedParentWorkspaceCustody
	found := false
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != runner.ContainedParentWorkspaceKind || event.Stage != scope.Event.Stage || event.Attempt != scope.Event.Attempt || event.Branch != scope.Event.Branch || event.Seq <= uint64(scope.Contract.PodAttempt) || event.Seq >= scope.Event.Seq {
			continue
		}
		data, _ := json.Marshal(event.Runner["custody"])
		if found || json.Unmarshal(data, &receipt) != nil || receipt.Version != 1 || !reflect.DeepEqual(receipt.Origin, scope.Contract.ParentOrigin) || receipt.Workspace.OwnerRunID != scope.Contract.Identity.RunID {
			return receipt, invoke.ErrWorkspaceNotQuiescent
		}
		found = true
	}
	if !found {
		return receipt, errors.New("parent managed workspace custody missing")
	}
	return receipt, nil
}

func (p *parentStagePod) importRecovery(ctx context.Context, reader *journal.Reader, writer *journal.Run, scope parentPodScope, retained childpod.RetainedAttempt, digest string, workspace *worktree.Worktree, report dispatcher.Report) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	// Recheck while owning the journal: a live writer cannot race scope selection.
	pending, err := parentPodCustodyPending(reader, digest)
	if err != nil || !pending {
		return errors.Join(invoke.ErrWorkspaceNotQuiescent, err)
	}
	c := scope.Contract
	store := childpod.ParentBlobs{RunDir: reader.Dir(), Identity: c.Identity}
	scoped := childpod.ParentAttemptBlobs{Store: store, ContractDigest: digest}
	recorder, err := runner.OwnedBranchRecorder(writer, scope.Event.Branch)
	if err != nil {
		return err
	}
	request := childpod.Request{ParentOrigin: c.ParentOrigin, Identity: c.Identity, Attempt: retained.Input.Attempt, Eligible: retained.Input.Eligible, Workspace: &childpod.WorkspaceInput{Path: workspace.Path}, Ceiling: c.Ceiling, StartedAt: c.StartedAt}
	executor := childpod.Executor{Blobs: scoped, Surrenders: p.surrenders, Recorder: recorder, RecoveryReader: reader, KeepContribution: func(_ context.Context, request childpod.Request, ref journal.Ref) error {
		return runner.RecordParentContribution(recorder, *request.Attempt.Envelope, request.Attempt.ChildExecutionDigest, ref)
	}}
	out, err := executor.Reconcile(ctx, request, retained, report)
	if err != nil {
		return err
	}
	if err = adoptContainedPodOutputs(ctx, recorder, scoped, &out); err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	custody, err := parentHeldReceipt(events, scope)
	if err != nil {
		return err
	}
	if err = runner.RecordContainedParentRecovery(writer, scope.Event, digest, custody, *retained.Input.Attempt.Envelope, out.Result.Transcript, out.ObservedUsage); err != nil {
		return err
	}
	if err = p.recoverAcceptedWait(ctx, writer, *retained.Input.Attempt.Envelope); err != nil {
		return err
	}
	b := &parentInvocationBlobs{ParentBlobs: store, contract: c, contractDigest: digest, recorder: recorder}
	return b.record(parentPodWriterJoined)
}
