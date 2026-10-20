package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
)

const localParentAuthenticationKind = "trusted-local.parent.authentication"

type trustedLocalParent struct {
	source   parentStagePod
	ordinary invoke.Goober
}

func (p *trustedLocalParent) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, childworkflow.ErrAuthorityUnavailable
}

func (p *trustedLocalParent) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if !trustedLocalChildren(p.source.service.config) {
		return apiv1.ResultEnvelope{}, childworkflow.ErrAuthorityUnavailable
	}
	if env.Goober == "" {
		env.Goober = p.source.goober
	}
	request, pin, release, reader, err := p.source.prepare(ctx, env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	defer release()
	if !pin.Self {
		return apiv1.ResultEnvelope{}, childworkflow.ErrAuthorityUnavailable
	}
	kit, binding, err := p.guidance(ctx, env, request.Ceiling)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	ordinary, ok := p.ordinary.(*harness.Executor)
	if !ok {
		return apiv1.ResultEnvelope{}, errors.New("trusted-local parent requires the source-bound local harness executor")
	}
	ordinary = ordinary.WithInvocationGuidance(kit.Instructions[env.Goober], kit.Goobers[env.Goober].Harness, workflow.ResolvedSkillFiles(kit.Goobers[env.Goober], kit.SkillPackages))
	if err := recordLocalParentAuthentication(p.source.journal, reader, env, *binding); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	ctx, err = credentials.WithChildCeiling(ctx, request.Ceiling)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	blobs := childpod.ParentBlobs{RunDir: p.source.journal.Dir(), Identity: p.source.identity}
	local := childpod.LocalExecutor{Root: filepath.Join(p.source.journal.Dir(), "trusted-local-attempts"), Recorder: p.source.journal, Blobs: blobs, ModelRef: binding.ModelRef}
	if binding.Ambient != nil {
		local.Authentication = []credentials.TrustedLocalAuthentication{*binding.Ambient}
	}
	local.BeforeStart = func(ctx context.Context, r childpod.Request, c childpod.Contract, digest string) error {
		// A fresh lease rechecks applied policy immediately before physical launch;
		// release it before the agent can call tools or a pending reload can wait.
		release, err := p.source.admission(env)(ctx)
		if err != nil {
			return err
		}
		release()
		owner := parentInvocationBlobs{ParentBlobs: blobs, recorder: p.source.journal}
		if err := owner.verifyOwner(ctx, c); err != nil {
			return err
		}
		if err := blobs.BindContract(ctx, digest); err != nil {
			return err
		}
		return p.source.journal.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: c.Stage, Attempt: c.Attempt, Branch: c.ParentBranch, Runner: map[string]any{"kind": childpod.ParentWriterStarted, "contractDigest": digest, "backend": "trusted-local"}})
	}
	local.Returned = func(_ context.Context, r childpod.Request, ref journal.Ref) error {
		if err := runner.RecordParentContribution(p.source.journal, *r.Attempt.Envelope, r.Attempt.ChildExecutionDigest, ref); err != nil {
			return err
		}
		return p.source.journal.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: r.Attempt.Stage, Attempt: r.Attempt.Number, Branch: r.ParentBranch, Runner: map[string]any{"kind": childpod.ParentWriterJoined, "contractDigest": r.Attempt.ChildExecutionDigest, "backend": "trusted-local"}})
	}
	out, err := local.Execute(ctx, request, func(ctx context.Context, workspace string) (dispatcher.SurrenderedResult, error) {
		localEnv := env
		localEnv.Workspace = workspace
		result, err := ordinary.Invoke(ctx, localEnv)
		return dispatcher.SurrenderedResult{Result: result}, err
	})
	return out.Result, err
}

func (p *trustedLocalParent) guidance(ctx context.Context, env apiv1.InvocationEnvelope, ceiling credentials.ChildCeiling) (*agentickit.Kit, *trustedLocalAuthenticationBinding, error) {
	lease, err := p.source.service.children.AcquirePreparedStage(ctx, env)
	if err != nil {
		return nil, nil, err
	}
	defer lease.Release()
	snapshot, release, err := (parentKitWriter{service: p.source.service, identity: p.source.identity}).snapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	kit, err := (agenticKitWriter{registrar: p.source.service.shared}).buildSnapshotKit(p.source.service.layout, snapshot, env, agentickit.ModeInvoke)
	if err != nil {
		return nil, nil, err
	}
	if err := narrowAgenticKitCredentials(kit, ceiling); err != nil {
		return nil, nil, err
	}
	if err := childworkflow.AttachParentAuthoringSkill(kit, lease.Authority); err != nil {
		return nil, nil, err
	}
	var grants []credentials.Grant
	for _, grant := range kit.Grants {
		grants = append(grants, credentials.Grant{Goober: grant.Goober, Capability: grant.Capability, Ref: grant.Ref})
	}
	binding, err := trustedLocalAuthenticationFor(env.Goober, kit.Goobers[env.Goober], grants)
	if err != nil {
		return nil, nil, err
	}
	if err := lease.Verify(ctx); err != nil {
		return nil, nil, err
	}
	return kit, &binding, nil
}

func recordLocalParentAuthentication(rec runner.OwnedJournalRecorder, reader *journal.Reader, env apiv1.InvocationEnvelope, binding trustedLocalAuthenticationBinding) error {
	existing, found, err := readLocalParentAuthentication(reader, env.ChildWorkflowOrigin.StageOccurrence)
	if err != nil {
		return err
	}
	if found {
		if !reflect.DeepEqual(existing, binding) {
			return credentials.ErrChildAuthenticationIsolation
		}
		return nil
	}
	_, branch, err := runner.OwnedJournalScope(rec)
	if err != nil {
		return err
	}
	return rec.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: stageArtifactName(env.RunID, env.TaskID), Attempt: int(env.Attempt), Branch: branch, Runner: map[string]any{"kind": localParentAuthenticationKind, "stageOccurrence": env.ChildWorkflowOrigin.StageOccurrence, "authentication": binding}})
}

func readLocalParentAuthentication(reader *journal.Reader, occurrence string) (trustedLocalAuthenticationBinding, bool, error) {
	var binding trustedLocalAuthenticationBinding
	events, err := reader.Events()
	if err != nil {
		return binding, false, err
	}
	found := false
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != localParentAuthenticationKind || event.Runner["stageOccurrence"] != occurrence {
			continue
		}
		var next trustedLocalAuthenticationBinding
		data, err := json.Marshal(event.Runner["authentication"])
		if err != nil || json.Unmarshal(data, &next) != nil || (next.ModelRef == "") == (next.Ambient == nil) {
			return binding, false, credentials.ErrChildAuthenticationIsolation
		}
		if found && !reflect.DeepEqual(binding, next) {
			return binding, false, credentials.ErrChildAuthenticationIsolation
		}
		binding, found = next, true
	}
	return binding, found, nil
}
