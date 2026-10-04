package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/interactivesession"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/sessionops"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func (r *daemonSessionRuntime) build(ctx context.Context, t triggerqueue.SessionTurn, inputs sessioning.ExecutionInputs) (interactivesession.PreparedTurn, error) {
	e, release, err := r.load(ctx, t.Session.Gaggle, t.Session.Profile)
	if err != nil {
		return interactivesession.PreparedTurn{}, err
	}
	prepared, err := r.prepare(e, t, inputs)
	if err != nil {
		release()
		return interactivesession.PreparedTurn{}, err
	}
	prepared.Release = release
	return prepared, nil
}
func (r *daemonSessionRuntime) prepare(e *interactiveRestartExecution, t triggerqueue.SessionTurn, inputs sessioning.ExecutionInputs) (interactivesession.PreparedTurn, error) {
	raw, err := inputs.Validate(strings.TrimPrefix(t.Record.ID, "trigger-"), t.Session.Gaggle)
	if err != nil {
		return interactivesession.PreparedTurn{}, err
	}
	e.source.RunID = strings.TrimPrefix(t.Record.ID, "trigger-")
	e.source.Trigger = journal.Trigger{Kind: journal.TriggerSignal, Ref: "session:" + t.Session.ID + ":" + t.ID}
	e.source.Session = &journal.SessionLineage{Gaggle: t.Session.Gaggle, SessionID: t.Session.ID, TurnID: t.ID, MessageID: t.Message.ID, AcceptanceID: t.Record.ID, EnvelopeDigest: journal.Digest(t.Record.Payload), InputDigest: journal.Digest(raw)}
	if r.setup.RunnerRegistry == nil {
		return interactivesession.PreparedTurn{}, errors.New("interactive session owner registry unavailable")
	}
	base, _ := r.setup.RunnerRegistry.Resolve("", t.Session.Gaggle, nil)
	if base == nil {
		return interactivesession.PreparedTurn{}, errors.New("interactive session has no local runner")
	}
	factory := func(name string, rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Goober, error) {
		delegate, err := e.agentic(name, rec, reg)
		if err != nil {
			return nil, err
		}
		writer, ok := rec.(sessionEventRecorder)
		if !ok {
			return nil, errors.New("interactive session host journal writer missing")
		}
		return sessionGoober{Goober: delegate, writer: writer, identity: e.source, actor: *t.Message.Actor, operations: r.operations, readers: r.setup.SessionBacklogReader, writers: r.setup.SessionBacklogWriter, resolvers: r.setup.SessionBacklogResolver, repairers: r.setup.SessionPRRepair, directory: filepath.Join(e.layout.RunsDir(), e.source.RunID)}, nil
	}
	driver, err := base.ForSessionExecution(e.source, factory)
	if err != nil {
		return interactivesession.PreparedTurn{}, err
	}
	run := func(_ context.Context, lease *interactiveaccess.ExecutionLease, published func() error) error {
		untrack, owned := r.setup.RunnerRegistry.trackRunLease(e.source.RunID, e.source.Workflow, driver, true)
		if !owned {
			return errors.New("interactive session already has an execution owner")
		}
		defer untrack()
		runtime, closeRuntime, err := e.enterHumanRuntime(lease, e.setup.SharedRegistry)
		if err != nil {
			return err
		}
		_, runErr := driver.Start(runtime, runner.StartInput{RunID: e.source.RunID, Gaggle: e.source.Gaggle, Machine: e.machine, GooberDigest: e.source.GooberDigest, Trigger: e.source.Trigger, SessionInputs: &inputs, OnJournalPublished: published})
		if closeErr := closeRuntime(); closeErr != nil {
			return errors.Join(runErr, closeErr)
		}
		return errors.Join(runErr, e.finishSessionRuntime(lease.Context()))
	}
	return interactivesession.PreparedTurn{Identity: e.source, Run: run}, nil
}

type sessionEventRecorder interface {
	runner.ArtifactRecorder
	Append(journal.Event) error
}
type sessionGoober struct {
	invoke.Goober
	writer     sessionEventRecorder
	identity   journal.RunIdentity
	directory  string
	actor      sessioning.Actor
	operations *sessionops.Bridge
	readers    sessionops.ReaderFactory
	writers    sessionops.WriterFactory
	resolvers  sessionops.ResolverFactory
	repairers  sessionops.RepairFactory
}

func (g sessionGoober) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	stage, ok := strings.CutPrefix(env.TaskID, g.identity.RunID+":")
	if !ok || stage != "respond" || env.RunID != g.identity.RunID || env.InstanceID != g.identity.InstanceID || env.Gaggle != g.identity.Gaggle || env.ConfigGeneration != g.identity.ConfigGeneration || env.GooberDigest != g.identity.GooberDigest || env.ChildWorkflowOrigin != nil {
		return apiv1.ResultEnvelope{}, errors.New("interactive session invocation identity differs")
	}
	rd, err := journal.OpenReadOnly(g.directory)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	events, err := rd.EventsBounded(8<<20, 8192)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	_, joined, err := journal.SessionWriterEvidence(events, g.identity)
	if err != nil || !joined {
		return apiv1.ResultEnvelope{}, errors.Join(errors.New("interactive session previous writer remains unjoined"), err)
	}
	started, err := childPodStarted(rd, stage, int(env.Attempt), false)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	note := func(kind string, refs []journal.Ref) error {
		return g.writer.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: stage, Attempt: int(env.Attempt), Branch: started.Branch, Artifacts: refs, Runner: map[string]any{"kind": kind, "stageSequence": started.Seq, "inputDigest": g.identity.Session.InputDigest}})
	}
	ctx, closeOperations, err := g.openOperations(ctx, env, started)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	defer closeOperations()
	if err = note(journal.SessionWriterStarted, nil); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	ctx, proof := invoke.WithWorkspaceQuiescence(ctx)
	result, runErr := g.Goober.Invoke(ctx, env)
	closeOperations()
	if err = proof.VerifyIdle(); err != nil {
		return result, errors.Join(runErr, err)
	}
	text := sessionResponseText(result.Summary, runErr)
	ref, err := g.writer.RecordArtifact(fmt.Sprintf("session-response/%d", started.Seq), []byte(text))
	if err != nil {
		return result, errors.Join(runErr, err)
	}
	return result, errors.Join(runErr, note(journal.SessionWriterJoined, []journal.Ref{ref}))
}
func (g sessionGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, errors.New("interactive session has no review gate")
}
