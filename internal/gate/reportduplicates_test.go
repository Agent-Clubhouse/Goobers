package gate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

type duplicateReportPoster struct {
	*fakeCommenter
	edits []string
}

func (p *duplicateReportPoster) UpdateComment(ctx context.Context, repo providers.RepositoryRef, id, body string) error {
	p.edits = append(p.edits, id)
	return p.fakeCommenter.UpdateComment(ctx, repo, id, body)
}

func TestDuplicateReportsTombstoneOnlyOwnExtrasKeepingEarliest(t *testing.T) {
	for _, kind := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderGitea} {
		t.Run(string(kind), func(t *testing.T) {
			repo := providers.RepositoryRef{Provider: kind, Name: "app"}
			p := &duplicateReportPoster{fakeCommenter: &fakeCommenter{}}
			apply := func() error {
				return UpsertFailureComment(context.Background(), p, repo, "42", 2, "ci-poll", "run-2", "", nil)
			}
			if err := apply(); err != nil {
				t.Fatal(err)
			}
			body := p.comments[0].Body
			early, late := time.Now(), time.Now().Add(time.Hour)
			p.comments = []providers.Comment{
				{ID: "20", AuthorID: "report-bot-id", Body: body, CreatedAt: &late},
				{ID: "1", AuthorID: "foreign-guid", Body: body, CreatedAt: &early},
				{ID: "10", AuthorID: "report-bot-id", Body: body, CreatedAt: &early, URL: "https://forge.example/comments/10"},
				{ID: "30", AuthorID: "report-bot-id", Body: body, CreatedAt: &late},
				{ID: "40", AuthorID: "report-bot-id", Body: failureStreakBody(1, "ci-poll", "run-1", "", failurePark{})},
			}
			if err := apply(); err != nil {
				t.Fatal(err)
			}
			if len(p.edits) != 2 || p.calls != 1 {
				t.Fatalf("edits=%v creates=%d", p.edits, p.calls)
			}
			for _, i := range []int{0, 3} {
				got := p.comments[i].Body
				if !strings.Contains(got, "Duplicate of [comment 10](https://forge.example/comments/10); superseded.") || strings.Contains(got, failureStreakMarker) || !strings.Contains(got, "goobers:terminal-intent") {
					t.Fatalf("invalid tombstone: %s", got)
				}
			}
			if p.comments[1].Body != body || p.comments[2].Body != body || !strings.Contains(p.comments[4].Body, "run-1") {
				t.Fatal("canonical, foreign, or unrelated comment changed")
			}
			if err := apply(); err != nil {
				t.Fatal(err)
			}
			if len(p.edits) != 2 {
				t.Fatal("tombstones edited again")
			}
		})
	}
}

func TestDuplicateReportTombstoneQuotaDoesNotCreate(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Name: "app"}
	p := &fakeCommenter{}
	apply := func() error {
		return UpsertFailureComment(context.Background(), p, repo, "42", 2, "ci-poll", "run-2", "", nil)
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	p.comments[0].ID = "10"
	duplicate := p.comments[0]
	duplicate.ID = "20"
	p.comments = append(p.comments, duplicate)
	p.updateErr = &providers.RateLimitError{Provider: providers.ProviderGitHub, RetryAfterRaw: "60"}
	if _, ok := providers.AsRateLimitError(apply()); !ok {
		t.Fatal("cleanup quota lost")
	}
	if p.calls != 1 {
		t.Fatal("cleanup failure created another comment")
	}
	p.updateErr = nil
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.comments[1].Body, "superseded") {
		t.Fatal("cleanup not retried")
	}
}

type concurrentReportPoster struct{ fakeCommenter }

func (p *concurrentReportPoster) UpdateWorkItem(ctx context.Context, r providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	item, err := p.fakeCommenter.UpdateWorkItem(ctx, r)
	if err != nil {
		return item, err
	}
	p.comments[0].ID = "10"
	p.comments[0].URL = "https://forge.example/comments/10"
	duplicate := p.comments[0]
	duplicate.ID = "20"
	p.comments = append(p.comments, duplicate)
	return item, nil
}
func TestDuplicateReportVisibleAfterPublicationIsTombstoned(t *testing.T) {
	p := &concurrentReportPoster{}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Name: "app"}
	if err := UpsertFailureComment(context.Background(), p, repo, "42", 2, "ci-poll", "run-2", "", nil); err != nil {
		t.Fatal(err)
	}
	if len(p.comments) != 2 || !strings.Contains(p.comments[0].Body, failureStreakMarker) || !strings.Contains(p.comments[1].Body, "superseded") {
		t.Fatalf("publication readback did not reconcile duplicate: %+v", p.comments)
	}
}
