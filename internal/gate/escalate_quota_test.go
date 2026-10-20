package gate

import (
	"context"
	"errors"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestUpsertFailureCommentQuotaDoesNotCreateDuplicate(t *testing.T) {
	for _, provider := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
		t.Run(string(provider), func(t *testing.T) {
			quota := &providers.RateLimitError{Provider: provider, Status: 429, RetryAfterRaw: "60"}
			poster := &fakeCommenter{updateErr: quota, comments: []providers.Comment{{ID: "existing", Body: failureStreakBody(1, "", "run-1", "", failurePark{})}}}
			repo := providers.RepositoryRef{Provider: provider, Name: "app"}
			err := UpsertFailureComment(context.Background(), poster, repo, "42", 2, "ci-poll", "run-2", "", nil)
			if _, ok := providers.AsRateLimitError(err); !ok {
				t.Fatalf("lost quota error: %v", err)
			}
			if poster.calls != 0 || len(poster.comments) != 1 {
				t.Fatalf("created duplicate: calls=%d comments=%d", poster.calls, len(poster.comments))
			}
			poster.updateErr = nil
			if err := UpsertFailureComment(context.Background(), poster, repo, "42", 2, "ci-poll", "run-2", "", nil); err != nil {
				t.Fatal(err)
			}
			if poster.calls != 0 || len(poster.comments) != 1 {
				t.Fatal("replay created duplicate")
			}
		})
	}
}

func TestUpsertFailureCommentReplayFindsAlreadyPublishedADOVersion(t *testing.T) {
	body := failureStreakBody(2, "ci-poll", "run-2", "", failurePark{})
	stamped, err := providers.StampAttribution(body, providers.Attribution{Schema: 1, Goobers: true, Gaggle: "example", Goober: "runner", Workflow: "implementation", Task: "ci-poll", Run: "run-2"}, "comment")
	if err != nil {
		t.Fatal(err)
	}
	poster := &fakeCommenter{updateErr: errors.New("editing unsupported"), comments: []providers.Comment{
		{ID: "old", Body: failureStreakBody(1, "ci-poll", "run-1", "", failurePark{})}, {ID: "committed", Body: stamped},
	}}
	if err := UpsertFailureComment(context.Background(), poster, providers.RepositoryRef{Provider: providers.ProviderADO}, "42", 2, "ci-poll", "run-2", "", nil); err != nil {
		t.Fatal(err)
	}
	if poster.calls != 0 {
		t.Fatal("replay created a duplicate after acknowledged-lost publication")
	}
}
