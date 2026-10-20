package main

import (
	"context"
	"errors"
	"path/filepath"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runner"
)

func (s *daemonCredentialService) installTrustedLocalChildren(journals *livejournal.Writer) {
	if !trustedLocalChildren(s.config) || journals == nil {
		return
	}
	s.parentBorrowJournal = journals.Adopt
	s.localParentExecutors = func(goober string, rec runner.ArtifactRecorder, ordinary invoke.Goober) (invoke.Goober, error) {
		owned, _, err := runner.OwnedJournalScope(rec)
		if err != nil {
			return nil, err
		}
		reader, err := journal.OpenReadOnly(owned.Dir())
		if err != nil {
			return nil, err
		}
		id, err := reader.Identity()
		if err != nil {
			return nil, err
		}
		if id.Child != nil {
			return nil, errors.New("child cannot acquire a local parent executor")
		}
		return &trustedLocalParent{source: parentStagePod{service: s, journal: owned, identity: id, goober: goober}, ordinary: ordinary}, nil
	}
	s.parentRecovery = func(ctx context.Context, id journal.RunIdentity) error {
		dir, err := s.layout.FindRunDir(id.RunID)
		if err != nil {
			return err
		}
		reader, err := journal.OpenReadOnly(dir)
		if err != nil {
			return err
		}
		// A daemon restart does not recover process custody from a PID. Missing
		// writer acknowledgement blocks resume until an independent owner resolves it.
		return childpod.VerifyParentCustody(ctx, reader)
	}
	permit := make(chan struct{}, 1)
	s.childExecutors = func(ctx context.Context, start childExecutionStart, runtime preparedChildRuntime) (runner.ChildExecutionFactories, error) {
		if !trustedLocalChildren(s.config) || start.Proposal == nil {
			return runner.ChildExecutionFactories{}, errors.New("trusted-local child placement unavailable")
		}
		if err := admitChildPodStages(start); err != nil {
			return runner.ChildExecutionFactories{}, err
		}
		for _, pin := range start.Proposal.Placements {
			if !pin.Self {
				return runner.ChildExecutionFactories{}, errors.New("trusted-local child cannot substitute for distributed placement")
			}
		}
		binding, err := s.localChildAuthentication(ctx, start)
		if err != nil {
			return runner.ChildExecutionFactories{}, err
		}
		ordinary, err := runtime.runner.TrustedLocalChildFactories()
		if err != nil {
			return runner.ChildExecutionFactories{}, err
		}
		source := childPodFactory{service: s, start: start, runtime: runtime, trustedLocal: true}
		factories := runner.ChildExecutionFactories{TrustedLocal: true, BorrowJournal: journals.Adopt, VerifyTerminalCustody: func(jr *journal.Run) error {
			reader, err := journal.OpenReadOnly(jr.Dir())
			if err != nil {
				return err
			}
			pending, _, err := s.pendingChildPodScopes(context.Background(), reader)
			if err != nil || len(pending) != 0 {
				return errors.Join(invoke.ErrChildCustodyPending, err)
			}
			return nil
		}}
		factories.NewDeterministic = func(rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Deterministic, error) {
			stage, err := source.executor(rec, "")
			if err != nil {
				return nil, err
			}
			executor, err := ordinary.NewDeterministic(rec, reg)
			if err != nil {
				return nil, err
			}
			return &trustedLocalChild{source: stage, deterministic: executor, binding: binding, permit: permit}, nil
		}
		factories.NewAgentic = func(goober string, rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Goober, error) {
			stage, err := source.executor(rec, goober)
			if err != nil {
				return nil, err
			}
			executor, err := ordinary.NewAgentic(goober, rec, reg)
			if err != nil {
				return nil, err
			}
			return &trustedLocalChild{source: stage, agent: executor, binding: binding, permit: permit}, nil
		}
		return factories, nil
	}
}

type trustedLocalChild struct {
	source        *childStagePod
	deterministic invoke.Deterministic
	agent         invoke.Goober
	binding       trustedLocalAuthenticationBinding
	permit        chan struct{}
}

func (p *trustedLocalChild) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	action, err := childPublicationAction(&run)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if action != "" {
		return p.source.publish(ctx, env, run, action)
	}
	out, err := p.execute(ctx, env, &run, false)
	return out.Result, err
}
func (p *trustedLocalChild) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	out, err := p.execute(ctx, env, nil, false)
	return out.Result, err
}
func (p *trustedLocalChild) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	out, err := p.execute(ctx, env, nil, true)
	if err != nil {
		return apiv1.Verdict{}, err
	}
	if out.Verdict == nil {
		return apiv1.Verdict{}, errors.New("local reviewer returned no verdict")
	}
	return *out.Verdict, nil
}

func (p *trustedLocalChild) execute(ctx context.Context, env apiv1.InvocationEnvelope, run *apiv1.DeterministicRun, review bool) (dispatcher.SurrenderedResult, error) {
	// Only physical child invocations hold the shared permit. A waiting parent
	// never occupies it; its existing scheduler capacity handoff remains intact.
	select {
	case p.permit <- struct{}{}:
		defer func() { <-p.permit }()
	case <-ctx.Done():
		return dispatcher.SurrenderedResult{}, ctx.Err()
	}
	if !trustedLocalChildren(p.source.service.config) {
		return dispatcher.SurrenderedResult{}, errors.New("trusted-local instance placement changed")
	}
	if env.Goober == "" {
		env.Goober = p.source.goober
	}
	request, pin, _, err := p.source.prepare(ctx, env, run, review)
	if err != nil {
		return dispatcher.SurrenderedResult{}, err
	}
	if !pin.Self {
		return dispatcher.SurrenderedResult{}, errors.New("local invocation has no pinned self placement")
	}
	blobs := childpod.ScopedBlobs{Queue: p.source.service.childQueue, Identity: p.source.start.Child.Identity}
	local := childpod.LocalExecutor{Root: filepath.Join(p.source.journal.Dir(), "trusted-local-attempts"), Recorder: p.source.journal, Blobs: blobs, ModelRef: p.binding.ModelRef}
	if run == nil && p.binding.Ambient != nil {
		local.Authentication = []credentials.TrustedLocalAuthentication{*p.binding.Ambient}
	}
	local.BeforeStart = func(ctx context.Context, r childpod.Request, c childpod.Contract, digest string) error {
		release, err := p.source.admit(ctx)
		if err != nil {
			return err
		}
		release()
		if err := blobs.BindContract(ctx, digest); err != nil {
			return err
		}
		return p.source.journal.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: c.Stage, Attempt: c.Attempt, Branch: c.ChildBranch, Runner: map[string]any{"kind": childPodWriterStarted, "contractDigest": digest, "backend": "trusted-local"}})
	}
	local.Returned = func(_ context.Context, r childpod.Request, _ journal.Ref) error {
		return p.source.journal.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: r.Attempt.Stage, Attempt: r.Attempt.Number, Branch: r.ChildBranch, Runner: map[string]any{"kind": childPodWriterJoined, "contractDigest": r.Attempt.ChildExecutionDigest, "backend": "trusted-local"}})
	}
	out, err := local.Execute(ctx, request, func(ctx context.Context, workspace string) (dispatcher.SurrenderedResult, error) {
		localEnv := env
		localEnv.Workspace = workspace
		if run != nil {
			result, err := p.deterministic.Run(ctx, localEnv, *run)
			return dispatcher.SurrenderedResult{Result: result}, err
		}
		if review {
			verdict, err := p.agent.Review(ctx, localEnv)
			return dispatcher.SurrenderedResult{Verdict: &verdict}, err
		}
		result, err := p.agent.Invoke(ctx, localEnv)
		return dispatcher.SurrenderedResult{Result: result}, err
	})
	return out, errors.Join(err, p.source.deferCancelledOutcome(ctx))
}

// Validate the exact parent binding against every selected source profile before
// building a generated runtime, so even config reconstruction cannot probe a
// sibling profile's unrelated authentication. The same check runs on rebuild.
func (s *daemonCredentialService) localChildAuthentication(ctx context.Context, start childExecutionStart) (trustedLocalAuthenticationBinding, error) {
	var empty trustedLocalAuthenticationBinding
	dir, err := s.layout.FindRunDir(start.Envelope.ParentRunID)
	if err != nil {
		return empty, err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return empty, err
	}
	binding, found, err := readLocalParentAuthentication(reader, start.Envelope.StageOccurrence)
	if err != nil {
		return empty, err
	}
	if !found {
		return empty, credentials.ErrChildAuthenticationIsolation
	}
	store, err := executionGenerationStore(s.layout)
	if err != nil {
		return empty, err
	}
	directory, lease, err := store.Acquire(ctx, start.Envelope.ConfigGeneration)
	if err != nil {
		return empty, err
	}
	defer func() { _ = lease.Release() }()
	set, _, err := loadConfigDirectory(directory)
	if err != nil {
		return empty, err
	}
	goobers, err := resolveGoobersForGaggle(set, start.Envelope.Gaggle)
	if err != nil {
		return empty, err
	}
	_, grants, err := buildGaggleCredentials(s.config, s.stores, gaggleProjectRef(set, start.Envelope.Gaggle), gaggleBacklogRef(set, start.Envelope.Gaggle), nil, s.shared)
	if err != nil {
		return empty, err
	}
	var selected []string
	for _, task := range start.Proposal.Workflow.Spec.Tasks {
		if task.Goober != "" {
			selected = append(selected, task.Goober)
		}
	}
	for _, gate := range start.Proposal.Workflow.Spec.Gates {
		if gate.Agentic != nil {
			selected = append(selected, gate.Agentic.Goober)
		}
	}
	slices.Sort(selected)
	for _, name := range slices.Compact(selected) {
		spec, ok := goobers[name]
		if !ok {
			return empty, credentials.ErrChildAuthenticationIsolation
		}
		if err := binding.admitChild(name, spec, grants); err != nil {
			return empty, err
		}
	}
	return binding, nil
}
