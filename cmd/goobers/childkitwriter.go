package main

import (
	"context"
	"errors"
	"fmt"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

const maxChildKitBytes = 8 << 20

// childKitWriter uses the same kit composition as ordinary pods, but selects
// the exact retained generated source before resolving its single Goober.
type childKitWriter struct {
	service  *daemonCredentialService
	identity journal.RunIdentity
	blobs    blobstore.Store
	recorder runner.ArtifactRecorder
}

func (w childKitWriter) WriteKit(ctx context.Context, attempt dispatcher.Attempt) (string, error) {
	if w.service == nil || w.blobs == nil || w.recorder == nil || attempt.Envelope == nil {
		return "", errors.New("child kit custody unavailable")
	}
	launcher := &queuedChildLauncher{layout: w.service.layout, queue: w.service.childQueue, authority: w.service.children}
	start, release, err := launcher.admittedChildIdentity(ctx, w.identity)
	if err != nil {
		return "", err
	}
	defer release()
	if err := verifyChildKitAttempt(w.identity, start, attempt); err != nil {
		return "", err
	}
	snapshot, releaseSnapshot, err := w.snapshot(ctx, start)
	if err != nil {
		return "", err
	}
	defer releaseSnapshot()
	kit, err := (agenticKitWriter{registrar: w.service.shared}).buildSnapshotKit(w.service.layout, snapshot, *attempt.Envelope, kitModeFor(attempt))
	if err != nil {
		return "", err
	}
	kit.ReviewRequiresDiff = attempt.Review && runner.ReviewsImplementation(start.Proposal.Machine, attempt.Stage)
	if attempt.Review {
		if !slices.Equal(kit.Goobers[attempt.Envelope.Goober].Capabilities, attempt.Envelope.Capabilities) {
			return "", errors.New("child reviewer capabilities differ from pinned Goober")
		}
	}
	ceiling := start.Proposal.CredentialCeiling().ModelOnly()
	if err := narrowChildKit(kit, ceiling); err != nil {
		return "", err
	}
	// The pod chooses its private checkout after custody verification.
	kit.Envelope.Workspace = ""
	data, digest, err := agentickit.Marshal(kit)
	if err != nil {
		return "", err
	}
	if len(data) > maxChildKitBytes {
		return "", errors.New("child kit exceeds 8 MiB custody bound")
	}
	if _, err := launcher.retainedChildIdentity(ctx, w.identity); err != nil {
		return "", err
	}
	// A journal artifact gives this content the run's normal retention owner.
	ref, err := w.recorder.RecordArtifact(fmt.Sprintf("child-kit/%s-%d.json", attempt.Stage, attempt.Number), data)
	if err != nil {
		return "", err
	}
	if ref.Digest != digest {
		return "", errors.New("child kit changed during journal custody")
	}
	if err := w.blobs.Put(ctx, digest, data); err != nil {
		return "", err
	}
	return digest, nil
}

func (w childKitWriter) snapshot(ctx context.Context, start childExecutionStart) (*workerConfigSnapshot, func(), error) {
	store, err := executionGenerationStore(w.service.layout)
	if err != nil {
		return nil, nil, err
	}
	directory, lease, err := store.Acquire(ctx, w.identity.ConfigGeneration)
	if err != nil {
		return nil, nil, err
	}
	release := func() { _ = lease.Release() }
	set, report, err := loadConfigDirectory(directory)
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("child kit: load retained config: %w (%s)", err, validationIssueSummary(report))
	}
	set.Workflows = []apiv1.Workflow{*start.Proposal.Workflow.DeepCopy()}
	instructions, skills, err := loadSnapshotGooberInputs(directory, set)
	if err != nil {
		release()
		return nil, nil, err
	}
	snapshot := &workerConfigSnapshot{configDir: directory, generation: w.identity.ConfigGeneration, cfg: w.service.config, set: set,
		instructions: instructions, skillPackages: skills, digests: newGooberDigestIndex(w.service.config, set, instructions, skills)}
	digest, err := snapshot.gooberDigestFor(w.identity.Gaggle, w.identity.Workflow)
	if err != nil || digest != w.identity.GooberDigest {
		release()
		return nil, nil, errors.Join(errors.New("child kit Goober pin differs from admitted source"), err)
	}
	return snapshot, release, nil
}

func verifyChildKitAttempt(id journal.RunIdentity, start childExecutionStart, attempt dispatcher.Attempt) error {
	env := attempt.Envelope
	if err := verifyChildKitIdentity(id, attempt); err != nil {
		return err
	}
	if env.ChildWorkflowOrigin != nil {
		return errors.New("child kit attempt differs from admitted identity")
	}
	if attempt.Review {
		gate, ok := start.Proposal.Machine.Gate(attempt.Stage)
		if !ok || gate.Agentic == nil || gate.Agentic.Goober != env.Goober {
			return errors.New("child kit reviewer differs from pinned gate")
		}
	} else {
		task, ok := start.Proposal.Machine.Task(attempt.Stage)
		if !ok || task.Type != apiv1.TaskAgentic || task.Goober != env.Goober || !slices.Equal(task.Capabilities, env.Capabilities) {
			return errors.New("child kit Goober differs from pinned task")
		}
	}
	return nil
}

func verifyChildKitIdentity(id journal.RunIdentity, attempt dispatcher.Attempt) error {
	if !attempt.Agentic || attempt.RunID != id.RunID || attempt.Gaggle != id.Gaggle || attempt.Workflow != id.Workflow || attempt.Stage == "" || attempt.Number < 1 {
		return errors.New("child kit dispatch differs from admitted identity")
	}
	env := attempt.Envelope
	if env == nil || env.RunID != id.RunID || env.Gaggle != id.Gaggle || env.WorkflowID != id.Workflow || env.ConfigGeneration != id.ConfigGeneration || env.GooberDigest != id.GooberDigest || int(env.Attempt) != attempt.Number || stageArtifactName(id.RunID, env.TaskID) != attempt.Stage || env.TaskID == attempt.Stage || env.InstanceID != attempt.InstanceID {
		return errors.New("child kit envelope differs from admitted identity")
	}
	return nil
}

func narrowChildKit(kit *agentickit.Kit, ceiling credentials.ChildCeiling) error {
	if err := ceiling.Validate(); err != nil {
		return err
	}
	if ceiling.AllowPublication {
		return errors.New("isolated child publication is not supported")
	}
	for _, spec := range kit.Goobers {
		if spec.Harness == "" || spec.Harness == apiv1.HarnessCopilot || len(spec.MCPServers) != 0 {
			return errors.New("child kit requires separable model authentication without BYO MCP")
		}
	}
	kit.Grants = slices.DeleteFunc(kit.Grants, func(grant agentickit.Grant) bool { return !slices.Contains(ceiling.AllowedKeys, grant.Capability) })
	for key := range kit.EnvCapabilities {
		if !slices.Contains(ceiling.AllowedKeys, key) {
			delete(kit.EnvCapabilities, key)
		}
	}
	return nil
}

func (l *queuedChildLauncher) admittedChildIdentity(ctx context.Context, id journal.RunIdentity) (childExecutionStart, func(), error) {
	ref, err := l.retainedChildIdentity(ctx, id)
	if err != nil {
		return childExecutionStart{}, nil, err
	}
	authority, release, err := l.acquire(ctx, ref.Envelope)
	if err != nil {
		return childExecutionStart{}, nil, err
	}
	source, err := l.queue.ChildProposal(ctx, ref.Child.Identity)
	if err != nil {
		release()
		return childExecutionStart{}, nil, err
	}
	proposal, err := childworkflow.ValidateRetainedStart(authority, ref.Envelope, source.Source)
	if err != nil {
		release()
		return childExecutionStart{}, nil, err
	}
	return childExecutionStart{childExecutionRef: ref, Proposal: proposal}, release, nil
}
