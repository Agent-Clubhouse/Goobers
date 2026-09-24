package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func TestSelectedRevisionDispatchRefusesBeforeWorkspaceOrPodEffects(t *testing.T) {
	env := apiv1.InvocationEnvelope{WorkspaceRevision: &apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		CommitSHA:  strings.Repeat("a", 40),
	}}
	activities := &Activities{}
	_, localErr := activities.provisionWorkspace(context.Background(), &env, apiv1.WorkspaceScratch, false, "", "")
	_, podErr := activities.DispatchStage(context.Background(), DispatchStageInput{Envelope: env})
	for name, err := range map[string]error{"workerhost": localErr, "pod": podErr} {
		var applicationErr *temporal.ApplicationError
		if !errors.As(err, &applicationErr) || applicationErr.Type() != workspacerevision.CodeInvalid || !applicationErr.NonRetryable() {
			t.Errorf("%s admission error = %v", name, err)
		}
	}
}

func TestWorkspaceRevisionEngineRejectsUnsupportedAuthorityBeforeDownstreamDispatch(t *testing.T) {
	produce := detTask("produce", "consume")
	produce.ContinueOnError = true
	events := runEngineFixture(t, conformanceFixture{
		spec: fixtureSpec("produce", []apiv1.Task{produce, detTask("consume", wf.TerminalComplete)}, nil),
		script: map[string][]scriptedCall{"produce": {{result: apiv1.ResultEnvelope{
			Status: apiv1.ResultSuccess,
			WorkspaceRevision: &apiv1.WorkspaceRevision{
				Repository: apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
				CommitSHA:  strings.Repeat("a", 40),
			},
		}}}},
		wantEngineErr: true,
	}, "unsupported-revision")
	rejected := false
	for _, event := range events {
		if event.Type == journal.EventStageFinished || event.Stage == "consume" {
			t.Errorf("unsupported authority accepted or dispatched: %+v", event)
		}
		if event.Type == journal.EventError && event.Stage == "produce" && event.Error != nil && event.Error.Code == workspacerevision.CodeInvalid {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("refusal was not journaled")
	}
}
