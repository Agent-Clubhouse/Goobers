package main

import (
	"encoding/json"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

var feedbackTestRepo = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}

func feedbackTestEvidence() feedbackEvidence {
	return feedbackEvidence{
		pr: providers.PullRequestSummary{Number: 77, State: "open", HeadSHA: revisionSelectedSHA},
		threads: providers.PullRequestReviewThreads{
			Reviews: []providers.PullRequestNativeReview{
				{ID: 2, Author: "reviewer", State: "CHANGES_REQUESTED", Body: "Please guard the write."},
				{ID: 3, Author: "reviewer", State: "APPROVED", Body: ""},
			},
			InlineComments: []providers.PullRequestInlineComment{
				{ID: 101, ThreadID: "T1", Author: "reviewer", Body: "Guard this.", Path: "a.go", URL: "https://x/101"},
				{ID: 102, ThreadID: "T1", Author: "human", Body: "Agreed.", InReplyTo: 101},
				{ID: 201, ThreadID: "T2", Author: "reviewer", Body: "Rename this.", IsResolved: true},
			},
		},
		comments: []providers.Comment{
			{ID: "11", Author: "human", Body: "One more thing."},
			{ID: "12", Author: "goobers-bot", Body: "verdict\n<!-- goobers:merge-review-status -->"},
			{ID: "13", Author: "ci-bot", AuthorType: "Bot", Body: "coverage 90%"},
		},
	}
}

func buildTestSnapshot(ev feedbackEvidence) apiv1.PRFeedbackSnapshot {
	return buildFeedbackSnapshot(feedbackTestRepo, "77", ev, time.Unix(0, 0))
}

func TestFeedbackSnapshotCanonicalContent(t *testing.T) {
	s := buildTestSnapshot(feedbackTestEvidence())
	if s.Schema != apiv1.PRFeedbackSnapshotVersion || s.HeadSHA != revisionSelectedSHA || !s.Complete || s.PullRequest != "77" {
		t.Fatalf("snapshot header = %+v", s)
	}
	if len(s.GeneralComments) != 1 || s.GeneralComments[0].ID != "11" {
		t.Fatalf("general comments = %+v, want only the human one (marker and bot comments excluded)", s.GeneralComments)
	}
	if len(s.Reviews) != 1 || s.Reviews[0].ID != "2" {
		t.Fatalf("reviews = %+v, want only the review with feedback text", s.Reviews)
	}
	if len(s.ReviewThreads) != 2 || s.ReviewThreads[0].ThreadID != "T1" || len(s.ReviewThreads[0].Comments) != 2 ||
		s.ReviewThreads[0].Comments[1].InReplyTo != "101" || !s.ReviewThreads[1].Resolved {
		t.Fatalf("threads = %+v", s.ReviewThreads)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSchemaJSON("pr-feedback-snapshot-v1.schema.json", data); err != nil {
		t.Fatalf("snapshot does not validate against its schema: %v", err)
	}
}

// TestFeedbackSnapshotDigestIsCanonical: equivalent provider payloads — any
// retrieval order, different URLs/anchors/display, CRLF line endings,
// surrounding whitespace, a changed capture time, a review state rewritten
// on push — produce the same digest.
func TestFeedbackSnapshotDigestIsCanonical(t *testing.T) {
	base := buildTestSnapshot(feedbackTestEvidence())
	ev := feedbackTestEvidence()
	comments := ev.threads.InlineComments
	ev.threads.InlineComments = []providers.PullRequestInlineComment{comments[2], comments[1], comments[0]}
	ev.threads.InlineComments[2].URL = "https://mirror/101"
	ev.threads.InlineComments[2].Line = 99
	ev.threads.InlineComments[1].Body = "  Agreed.\r\n"
	ev.threads.Reviews[0].State = "DISMISSED"
	ev.comments = []providers.Comment{ev.comments[2], ev.comments[1], ev.comments[0]}
	ev.comments[2].URL = "https://mirror/11"
	again := buildFeedbackSnapshot(feedbackTestRepo, "77", ev, time.Now())
	if again.SnapshotDigest != base.SnapshotDigest {
		t.Fatalf("digest changed for an equivalent payload: %s vs %s", again.SnapshotDigest, base.SnapshotDigest)
	}
	if reasons := compareFeedbackSnapshot(base, again, feedbackCompareOptions{}); len(reasons) != 0 {
		t.Fatalf("equivalent payload reported stale: %+v", reasons)
	}
}

func TestFeedbackSnapshotDigestIncludesHeadAndCompleteness(t *testing.T) {
	base := buildTestSnapshot(feedbackTestEvidence())
	moved := feedbackTestEvidence()
	moved.pr.HeadSHA = revisionMovedSHA
	if buildTestSnapshot(moved).SnapshotDigest == base.SnapshotDigest {
		t.Fatal("digest ignores the head")
	}
	incomplete := base
	incomplete.Complete = false
	if feedbackSnapshotDigest(incomplete) == base.SnapshotDigest {
		t.Fatal("digest ignores completeness")
	}
}

func TestCompareFeedbackSnapshotReasons(t *testing.T) {
	base := buildTestSnapshot(feedbackTestEvidence())
	for _, tc := range []struct {
		name   string
		mutate func(ev *feedbackEvidence)
		opts   feedbackCompareOptions
		want   string
		id     string
	}{
		{name: "new general comment", mutate: func(ev *feedbackEvidence) {
			ev.comments = append(ev.comments, providers.Comment{ID: "14", Author: "human", Body: "Also this."})
		}, want: staleReasonNew, id: "14"},
		{name: "new reply on a thread", mutate: func(ev *feedbackEvidence) {
			ev.threads.InlineComments = append(ev.threads.InlineComments, providers.PullRequestInlineComment{ID: 103, ThreadID: "T1", Author: "human", Body: "Wait.", InReplyTo: 101})
		}, want: staleReasonNew, id: "103"},
		{name: "new thread", mutate: func(ev *feedbackEvidence) {
			ev.threads.InlineComments = append(ev.threads.InlineComments, providers.PullRequestInlineComment{ID: 301, ThreadID: "T3", Author: "human", Body: "New finding."})
		}, want: staleReasonNew, id: "T3"},
		{name: "new review body", mutate: func(ev *feedbackEvidence) {
			ev.threads.Reviews = append(ev.threads.Reviews, providers.PullRequestNativeReview{ID: 4, Author: "human", Body: "Rework this."})
		}, want: staleReasonNew, id: "4"},
		{name: "edited thread body with the same head", mutate: func(ev *feedbackEvidence) {
			ev.threads.InlineComments[0].Body = "Guard this, and log it."
		}, want: staleReasonChanged, id: "101"},
		{name: "edited general comment", mutate: func(ev *feedbackEvidence) { ev.comments[0].Body = "Two more things." },
			want: staleReasonChanged, id: "11"},
		{name: "deleted comment", mutate: func(ev *feedbackEvidence) { ev.comments = ev.comments[1:] },
			want: staleReasonMissing, id: "11"},
		{name: "deleted thread", mutate: func(ev *feedbackEvidence) { ev.threads.InlineComments = ev.threads.InlineComments[:2] },
			want: staleReasonMissing, id: "T2"},
		{name: "thread resolved outside the run", mutate: func(ev *feedbackEvidence) {
			ev.threads.InlineComments[0].IsResolved, ev.threads.InlineComments[1].IsResolved = true, true
		}, want: staleReasonThreadState, id: "T1"},
		{name: "resolved thread reopened", mutate: func(ev *feedbackEvidence) { ev.threads.InlineComments[2].IsResolved = false },
			want: staleReasonThreadState, id: "T2"},
		{name: "reopened after this run resolved it", mutate: func(*feedbackEvidence) {},
			opts: feedbackCompareOptions{resolvedByRun: map[string]bool{"T1": true}}, want: staleReasonThreadState, id: "T1"},
		{name: "head moved", mutate: func(ev *feedbackEvidence) { ev.pr.HeadSHA = revisionMovedSHA },
			want: staleReasonHead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := feedbackTestEvidence()
			tc.mutate(&ev)
			reasons := compareFeedbackSnapshot(base, buildTestSnapshot(ev), tc.opts)
			if staleInputCode(reasons) != tc.want {
				t.Fatalf("reasons = %+v, want leading %s", reasons, tc.want)
			}
			if tc.id != "" && reasons[0].ID != tc.id {
				t.Fatalf("reason id = %q, want %q (%+v)", reasons[0].ID, tc.id, reasons)
			}
		})
	}
}

// TestCompareFeedbackSnapshotIgnoresOwnAndSystemChanges covers the documented
// exclusions: this run's own marker-bearing replies, bot comments, the
// run resolving a thread it addressed, and outdated flips caused by a push.
func TestCompareFeedbackSnapshotIgnoresOwnAndSystemChanges(t *testing.T) {
	base := buildTestSnapshot(feedbackTestEvidence())
	ev := feedbackTestEvidence()
	ev.threads.InlineComments = append(ev.threads.InlineComments, providers.PullRequestInlineComment{
		ID: 150, ThreadID: "T1", Author: "goobers-bot", Body: "Addressed.\n\n<!-- goobers:review-thread-response:run:T1 -->",
	})
	ev.comments = append(ev.comments,
		providers.Comment{ID: "20", Author: "goobers-bot", Body: "account\n<!-- goobers:remediation-response:run -->"},
		providers.Comment{ID: "21", Author: "ci-bot", AuthorType: "Bot", Body: "coverage 91%"},
	)
	for i := range ev.threads.InlineComments {
		if ev.threads.InlineComments[i].ThreadID == "T1" {
			ev.threads.InlineComments[i].IsResolved = true
			ev.threads.InlineComments[i].IsOutdated = true
		}
	}
	ev.pr.HeadSHA = revisionPublishedSHA
	reasons := compareFeedbackSnapshot(base, buildTestSnapshot(ev), feedbackCompareOptions{
		expectedHead: revisionPublishedSHA, mayResolve: map[string]bool{"T1": true},
	})
	if len(reasons) != 0 {
		t.Fatalf("own/system changes reported stale: %+v", reasons)
	}
}

func TestCheckThreadFeedbackUsesAnExistingListing(t *testing.T) {
	base := buildTestSnapshot(feedbackTestEvidence())
	listing := feedbackTestEvidence().threads
	if reasons := checkThreadFeedback(&base, listing, feedbackCompareOptions{}); len(reasons) != 0 {
		t.Fatalf("unchanged listing reported stale: %+v", reasons)
	}
	listing.InlineComments[0].Body = "edited"
	if reasons := checkThreadFeedback(&base, listing, feedbackCompareOptions{}); staleInputCode(reasons) != staleReasonChanged {
		t.Fatalf("reasons = %+v, want changed_feedback", reasons)
	}
	if reasons := checkThreadFeedback(nil, listing, feedbackCompareOptions{}); reasons != nil {
		t.Fatalf("no recorded snapshot must never be stale, got %+v", reasons)
	}
}

func TestLessFeedbackIDOrdersNumericIDsNumerically(t *testing.T) {
	if !lessFeedbackID("9", "10") || lessFeedbackID("10", "9") || !lessFeedbackID("77/10/1", "77/9/1") {
		t.Fatal("lessFeedbackID ordering is wrong")
	}
}
