package gate

import (
	"context"
	"errors"
	"github.com/goobers/goobers/providers"
	"testing"
)

type review7671ChangingReader struct {
	*fakeCommenter
	readErr error
}

func (p *review7671ChangingReader) GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error) {
	return providers.WorkItem{}, p.readErr
}
func TestReview7671ADOReplayLostAckChangingLookup(t *testing.T) {
	p := &review7671ChangingReader{fakeCommenter: &fakeCommenter{err: errors.New("lost publication acknowledgement"), commitOnError: true, updateErr: errors.New("ADO comment editing unsupported")}, readErr: errors.New("temporary label lookup failure")}
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Name: "app"}
	apply := func() error {
		return UpsertFailureComment(context.Background(), p, repo, "42", 2, "ci-poll", "run-2", "", nil)
	}
	if err := apply(); err == nil {
		t.Fatal("expected lost acknowledgement")
	}
	if len(p.comments) != 1 {
		t.Fatalf("expected initial publication; got %d", len(p.comments))
	}
	p.err = nil
	p.readErr = nil
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if len(p.comments) != 1 {
		t.Fatalf("same durable intent published twice after label lookup recovered: comments=%d\nfirst=%s\nsecond=%s", len(p.comments), p.comments[0].Body, p.comments[1].Body)
	}
}

func TestTerminalIntentMarkerCannotBeForgedByAnotherADOAuthor(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Name: "app"}
	p := &fakeCommenter{updateErr: errors.New("editing unsupported")}
	apply := func() error {
		return UpsertFailureComment(context.Background(), p, repo, "42", 2, "ci-poll", "run-2", "", nil)
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	// A foreign identity copied the complete marker and our display name.
	p.comments[0].AuthorID = "someone-else"
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if p.calls != 2 {
		t.Fatal("foreign marker suppressed our publication")
	}
	// Matching our GUID remains valid even when our display name changes.
	p.comments[1].Author = "renamed bot"
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if p.calls != 2 {
		t.Fatal("our own marker was not recognized")
	}
}

func TestTerminalIntentIdentitiesAreDistinct(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "example", Project: "project", Name: "app"}
	original := failureIntentMarker(repo, "42", "work", "run-2", "ci-poll", 2, false)
	variants := []string{
		failureIntentMarker(repo, "42", "infra", "run-2", "ci-poll", 2, false),
		failureIntentMarker(repo, "42", "work", "run-3", "ci-poll", 2, false),
		failureIntentMarker(repo, "42", "work", "run-2", "build", 2, false),
		failureIntentMarker(repo, "42", "work", "run-2", "ci-poll", 3, false),
		failureIntentMarker(repo, "42", "work", "run-2", "ci-poll", 2, true),
		failureIntentMarker(repo, "43", "work", "run-2", "ci-poll", 2, false),
	}
	repo.Project = "other-project"
	variants = append(variants, failureIntentMarker(repo, "42", "work", "run-2", "ci-poll", 2, false))
	for _, marker := range variants {
		if marker == original {
			t.Fatal("different intent has same marker")
		}
	}
}

type reportLoginPoster struct {
	Commenter
	login string
	err   error
}

func (p reportLoginPoster) AuthenticatedLogin(context.Context) (string, error) { return p.login, p.err }
func TestTerminalIntentLoginTrustAndQuota(t *testing.T) {
	for _, kind := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderGitea} {
		t.Run(string(kind), func(t *testing.T) {
			repo := providers.RepositoryRef{Provider: kind, Name: "app"}
			fake := &fakeCommenter{updateErr: errors.New("editing unavailable")}
			p := reportLoginPoster{Commenter: fake, login: "report-bot"}
			apply := func() error {
				return UpsertFailureComment(context.Background(), p, repo, "42", 1, "ci-poll", "run-1", "", nil)
			}
			if err := apply(); err != nil {
				t.Fatal(err)
			}
			p.err = &providers.RateLimitError{Provider: kind, RetryAfterRaw: "60"}
			if _, ok := providers.AsRateLimitError(apply()); !ok {
				t.Fatal("identity lookup quota not retained")
			}
			if fake.calls != 1 {
				t.Fatal("identity failure caused duplicate")
			}
			p.err = nil
			fake.comments[0].Author = "foreign-user"
			if err := apply(); err != nil {
				t.Fatal(err)
			}
			if fake.calls != 2 {
				t.Fatal("foreign login suppressed report")
			}
			if err := apply(); err != nil {
				t.Fatal(err)
			}
			if fake.calls != 2 {
				t.Fatal("self login not recognized")
			}
		})
	}
}
