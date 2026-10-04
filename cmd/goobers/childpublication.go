package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/childpublication"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func childPublicationAction(run *apiv1.DeterministicRun) (string, error) {
	if run == nil || len(run.Command) < 2 || run.Command[0] != "goobers" {
		return "", nil
	}
	action := ""
	switch run.Command[1] {
	case "push-branch":
		action = "branch"
	case "open-pr":
		action = "pr"
	default:
		return "", nil
	}
	if len(run.Command) != 2 || run.Script != "" || len(run.Env) != 0 || run.Network != "" || run.SyncBase || run.InjectRunContext || (run.Workspace != "" && run.Workspace != apiv1.WorkspaceRepo) {
		return "", errors.New("child publication requires the canonical typed command without flags, shell, environment or workspace overrides")
	}
	return action, nil
}

func childPublicationCapability(task apiv1.Task, action string) (string, error) {
	candidates := []string{string(capability.RepoPush)}
	if action == "pr" {
		candidates = []string{string(capability.ProviderPRWrite), string(capability.GitHubPRWrite)}
	}
	for _, candidate := range candidates {
		if slices.Contains(task.Capabilities, candidate) {
			return candidate, nil
		}
	}
	return "", errors.New("child publication stage lacks its explicit provider capability")
}

func (p *childStagePod) publish(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun, action string) (apiv1.ResultEnvelope, error) {
	// The only local programs are host-owned Git object/transport operations:
	// private config, no checkout hooks or authored command, synchronous wait.
	if ack := invoke.RegisterWorkspaceWriter(ctx); ack != nil {
		defer ack(nil)
	}
	request, _, _, err := p.prepare(ctx, env, &run, false)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if request.Workspace == nil {
		return apiv1.ResultEnvelope{}, errors.New("child publication requires its retained managed fork")
	}
	launcher := &queuedChildLauncher{layout: p.service.layout, queue: p.service.childQueue, authority: p.service.children}
	authority, release, err := launcher.acquire(ctx, p.start.Envelope)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	defer release()
	source, err := p.service.childQueue.ChildProposal(ctx, p.start.Child.Identity)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	proposal, err := childworkflow.ValidateRetainedStart(authority, p.start.Envelope, source.Source)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	ceiling := proposal.CredentialCeiling()
	if !ceiling.AllowPublication || !sameChildCeiling(ceiling, p.start.Proposal.CredentialCeiling()) {
		return apiv1.ResultEnvelope{}, errors.New("child publication delegation unavailable")
	}
	task, ok := proposal.Machine.Task(request.Attempt.Stage)
	if !ok || !reflect.DeepEqual(task.Run, &run) || !slices.Equal(task.Capabilities, env.Capabilities) {
		return apiv1.ResultEnvelope{}, errors.New("child publication differs from retained stage")
	}
	key, err := childPublicationCapability(task, action)
	if err != nil || !slices.Contains(ceiling.AllowedKeys, key) {
		return apiv1.ResultEnvelope{}, errors.Join(errors.New("child publication capability was not delegated"), err)
	}
	if _, err = launcher.retainedChildIdentity(ctx, p.identity); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, childpublication.EffectTimeout)
	defer cancel()
	target, err := p.publicationTarget(authority, request.Attempt.Stage, request.Workspace.Path, request.Workspace.Fork)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	credential, scheme, err := p.publicationCredential(ctx, request.Attempt.Stage, key)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	factory := p.publicationProvider
	if p.service.childPublisher != nil {
		factory = p.service.childPublisher
	}
	publisher, err := factory(target, key, credential, scheme)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	var outputs map[string]any
	if action == "branch" {
		receipt, publishErr := publisher.Push(ctx, target)
		err = publishErr
		outputs = map[string]any{"pushed": err == nil, "branch": receipt.Head, "sha": receipt.SHA}
	} else {
		title, body, draft, textErr := p.publicationText(env)
		if textErr != nil {
			return apiv1.ResultEnvelope{}, textErr
		}
		receipt, publishErr := publisher.OpenPR(ctx, target, title, body, draft)
		err = publishErr
		outputs = map[string]any{"opened": err == nil, "prNumber": receipt.Number, "pull-request-url": receipt.URL}
	}
	auditErr := p.recordPublication(ctx, action, request.Attempt.Stage, request.Attempt.Number)
	if err != nil || auditErr != nil {
		return apiv1.ResultEnvelope{}, errors.Join(err, auditErr)
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Integrity: apiv1.IntegrityDerived, Outputs: outputs}, nil
}

func (p *childStagePod) publicationCredential(ctx context.Context, stage, key string) (httpapi.MintedCredential, string, error) {
	request := httpapi.CredentialResolveRequest{RunID: p.identity.RunID, Stage: stage, Capabilities: []string{key}}
	pinned, err := p.service.loadPinnedStage(ctx, request)
	if err != nil {
		return httpapi.MintedCredential{}, "", err
	}
	defer pinned.release()
	if !reflect.DeepEqual(pinned.identity, p.identity) {
		return httpapi.MintedCredential{}, "", errors.New("child publication credential identity changed")
	}
	// publish already holds the current-authority read lease. Reacquiring it
	// through public Resolve could deadlock behind a pending config reload.
	// The shared pinned-profile and materialization gates still apply in full.
	pinned.profile.implicitKeys = nil
	requested, err := gateRequestedCapabilities(pinned.profile, request)
	if err != nil {
		return httpapi.MintedCredential{}, "", err
	}
	resolved, err := p.service.mintStageCredentials(ctx, pinned, requested)
	if err != nil {
		return httpapi.MintedCredential{}, "", err
	}
	if err = p.service.journalResolution(pinned, request, stageResolveMode{marker: "child.publication.credentials"}, requested, resolved.materialized); err != nil {
		return httpapi.MintedCredential{}, "", err
	}
	response := resolved.response(request)
	for _, credential := range response.Credentials {
		if credential.Capability == key && credential.Value != "" {
			return credential, response.RepoAuthScheme, nil
		}
	}
	return httpapi.MintedCredential{}, "", errors.New("child host publication credential unavailable")
}

func (p *childStagePod) publicationText(env apiv1.InvocationEnvelope) (string, string, bool, error) {
	title := "Child workflow " + p.identity.RunID
	body := "Generated by child workflow " + p.identity.RunID + " for parent " + p.identity.Child.ParentRunID + "."
	draft := true
	for key, value := range env.Inputs {
		switch key {
		case "title":
			text, ok := value.(string)
			if !ok {
				return "", "", false, errors.New("child PR title must be text")
			}
			title = text
		case "body":
			text, ok := value.(string)
			if !ok {
				return "", "", false, errors.New("child PR body must be text")
			}
			body = text
		case "draft":
			enabled, ok := value.(bool)
			if !ok {
				return "", "", false, errors.New("child PR draft must be boolean")
			}
			draft = enabled
		default:
			return "", "", false, fmt.Errorf("unsupported child PR input %q", key)
		}
	}
	if p.service.shared == nil {
		return "", "", false, errors.New("child publication scrubber unavailable")
	}
	return string(p.service.shared.Scrub([]byte(title))), string(p.service.shared.Scrub([]byte(body))), draft, nil
}

func (p *childStagePod) recordPublication(ctx context.Context, action, stage string, attempt int) error {
	record, err := p.service.childQueue.ChildPublication(context.WithoutCancel(ctx), p.start.Child.Identity, action)
	if err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	ref, err := p.journal.RecordArtifactBoundedWithIntegrity("child-publication/"+action, data, apiv1.IntegrityDerived, 128<<10)
	if err != nil {
		return err
	}
	if err = p.journal.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: stage, Attempt: attempt, Artifacts: []journal.Ref{ref}, Runner: map[string]any{"kind": "child.publication.observed", "action": action, "state": record.State, "intentDigest": record.Digest, "parentRunId": p.identity.Child.ParentRunID, "stageOccurrence": p.identity.Child.StageOccurrence}}); err != nil {
		return err
	}
	if record.State != "confirmed" {
		return nil
	}
	touched := journal.ExternalRef{Provider: string(p.runtime.repoRef.Provider), Kind: action}
	if action == "branch" {
		var receipt childpublication.BranchReceipt
		if err = json.Unmarshal(record.Receipt, &receipt); err != nil {
			return err
		}
		touched.ID, touched.CommitSHA = receipt.Head, receipt.SHA
	} else {
		var receipt providers.PullRequestResult
		if err = json.Unmarshal(record.Receipt, &receipt); err != nil {
			return err
		}
		touched.ID, touched.URL = receipt.ID, receipt.URL
	}
	return p.journal.Append(journal.Event{Type: journal.EventRefTouched, Stage: stage, Attempt: attempt, ExternalRef: &touched})
}

func credentialExpiry(c httpapi.MintedCredential) time.Time {
	if c.ExpiresAt != nil {
		return *c.ExpiresAt
	}
	return time.Time{}
}
