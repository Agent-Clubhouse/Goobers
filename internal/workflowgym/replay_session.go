package workflowgym

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// recordedCommentBody stands in for a historical comment's text, which the
// journal does not retain; the virtual sink answers the request regardless.
const recordedCommentBody = "workflow gym replay of a recorded comment"

// recordedSessionAct replays the historical session's provider work through
// the sandbox with the production GitHub provider: it reads the fixture's work
// item, then re-issues every provider mutation the recorded journal projected
// as ref.touched (see recordedMutation for what counts). Comments are replayed
// against their recorded virtual sink; any other recorded mutation has no
// reconstructable request and is refused as unavailable. Every step runs even
// after a failure so the replay manifest reports each gap, and nothing ever
// leaves the sandbox.
func recordedSessionAct(fixture harness.BenchmarkFixture) harness.BenchmarkAct {
	return func(ctx context.Context, _ harness.RunRequest, sandbox *harness.BenchmarkSandbox) error {
		provider, err := sandbox.GitHubProvider()
		if err != nil {
			return err
		}
		repo := providers.RepositoryRef{Owner: fixture.Provider.Repository.Owner, Name: fixture.Provider.Repository.Name}
		var errs []error
		if id := fixture.Provider.Issue; id != "" {
			_, err := provider.GetWorkItem(ctx, repo, id)
			errs = append(errs, err)
		}
		if id := fixture.Provider.PullRequest; id != "" {
			_, err := provider.GetPullRequest(ctx, repo, id)
			errs = append(errs, err)
		}
		for _, event := range fixture.Events {
			if !recordedMutation(event) {
				continue
			}
			errs = append(errs, replayRecordedMutation(ctx, sandbox, provider, repo, fixture.Provider.Provider, event))
		}
		return errors.Join(errs...)
	}
}

// recordedMutation reports whether a journal event records a provider
// mutation that took effect. The run-level branch record (ref.touched with
// kind "branch", no stage and no operation; internal/runner recordRunBranch
// and internal/engine recordRunBranch) is run provenance naming the workspace
// branch, not a mutation. A ref.touched whose recorded outcome is anything but
// success did not take effect historically, so replay must not re-issue it.
func recordedMutation(event journal.Event) bool {
	if event.Type != journal.EventRefTouched || event.ExternalRef == nil {
		return false
	}
	operation, _ := event.Runner["operation"].(string)
	if event.ExternalRef.Kind == "branch" && operation == "" && event.Stage == "" {
		return false
	}
	outcome, _ := event.Runner["outcome"].(string)
	return outcome == "" || outcome == "success"
}

func replayRecordedMutation(ctx context.Context, sandbox *harness.BenchmarkSandbox, provider *providers.GitHubProvider,
	repo providers.RepositoryRef, fixtureProvider string, event journal.Event,
) error {
	ref := *event.ExternalRef
	operation, _ := event.Runner["operation"].(string)
	effect := recordedMutationEffect(operation, ref.Kind)
	target := recordedMutationTarget(operation, ref)
	if ref.Provider != fixtureProvider {
		return sandbox.RefuseUnavailable(effect, target,
			fmt.Sprintf("recorded mutation on provider %q is outside the %q fixture", ref.Provider, fixtureProvider))
	}
	if operation == "comment" && (ref.Kind == "issue" || ref.Kind == "pr") && ref.ID != "" {
		_, err := provider.CreateWorkItemComment(ctx, repo, ref.ID, recordedCommentBody)
		return err
	}
	return sandbox.RefuseUnavailable(effect, target,
		fmt.Sprintf("recorded %s mutation has no replayable request", effect))
}

// recordedMutationTarget names a recorded mutation as
// "[operation ]provider kind#id", omitting whatever the journal left empty.
func recordedMutationTarget(operation string, ref journal.ExternalRef) string {
	var parts []string
	for _, part := range []string{operation, ref.Provider} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	item := ref.Kind
	if ref.ID != "" {
		item += "#" + ref.ID
	}
	if item != "" {
		parts = append(parts, item)
	}
	return strings.Join(parts, " ")
}

// recordedMutationEffect classifies a recorded mutation by its operation, or
// by its ref kind when the journal recorded no operation (as for a published
// child branch).
func recordedMutationEffect(operation, kind string) harness.BenchmarkEffect {
	switch {
	case operation == "comment" || operation == "review-thread-reply":
		return harness.BenchmarkEffectComment
	case operation == "merge" || operation == "enqueue":
		return harness.BenchmarkEffectMerge
	case operation == "branch" || operation == "commit" || operation == "update-branch",
		operation == "" && kind == "branch":
		return harness.BenchmarkEffectGitPush
	default:
		return harness.BenchmarkEffectProviderWrite
	}
}
