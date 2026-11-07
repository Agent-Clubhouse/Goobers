package workflowgym

import (
	"context"
	"errors"
	"fmt"

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
// as ref.touched. Comments are replayed against their recorded virtual sink;
// any other recorded mutation has no reconstructable request and is refused
// as unavailable. Every step runs even after a failure so the replay manifest
// reports each gap, and nothing ever leaves the sandbox.
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
			if event.Type != journal.EventRefTouched || event.ExternalRef == nil {
				continue
			}
			errs = append(errs, replayRecordedMutation(ctx, sandbox, provider, repo, fixture.Provider.Provider, event))
		}
		return errors.Join(errs...)
	}
}

func replayRecordedMutation(ctx context.Context, sandbox *harness.BenchmarkSandbox, provider *providers.GitHubProvider,
	repo providers.RepositoryRef, fixtureProvider string, event journal.Event,
) error {
	ref := *event.ExternalRef
	operation, _ := event.Runner["operation"].(string)
	target := fmt.Sprintf("%s %s %s#%s", operation, ref.Provider, ref.Kind, ref.ID)
	if ref.Provider != fixtureProvider {
		return sandbox.RefuseUnavailable(recordedMutationEffect(operation), target,
			fmt.Sprintf("recorded mutation on provider %q is outside the %q fixture", ref.Provider, fixtureProvider))
	}
	if operation == "comment" && (ref.Kind == "issue" || ref.Kind == "pr") && ref.ID != "" {
		_, err := provider.CreateWorkItemComment(ctx, repo, ref.ID, recordedCommentBody)
		return err
	}
	return sandbox.RefuseUnavailable(recordedMutationEffect(operation), target,
		fmt.Sprintf("recorded %q mutation has no replayable request", operation))
}

func recordedMutationEffect(operation string) harness.BenchmarkEffect {
	switch {
	case operation == "comment" || operation == "review-thread-reply":
		return harness.BenchmarkEffectComment
	case operation == "merge" || operation == "enqueue":
		return harness.BenchmarkEffectMerge
	case operation == "branch" || operation == "commit" || operation == "update-branch":
		return harness.BenchmarkEffectGitPush
	default:
		return harness.BenchmarkEffectProviderWrite
	}
}
