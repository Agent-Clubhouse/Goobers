package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/prstatus"
	"github.com/goobers/goobers/providers"
)

func shadowEvents(t *testing.T, root string) []journal.Event {
	t.Helper()
	events, err := journal.ReadInstanceLog(layoutFor(root).SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var out []journal.Event
	for _, event := range events {
		if event.Reason == "merge-verdict-shadow" {
			out = append(out, event)
		}
	}
	return out
}

func TestMergeVerdictShadowProductionDecisions(t *testing.T) {
	for _, tc := range []struct {
		name, state, summary, label, raw, phase, reason, outcome, baseMatch string
		advisory, draft, explicit, annotationFails                          bool
		wantMerge, wantCode                                                 int
		wantPresence                                                        string
		wouldBlock                                                          bool
	}{
		{name: "missing-structured", wantMerge: 1, wantPresence: "absent", wouldBlock: true, outcome: "landing-submitted"},
		{name: "missing-explicit", explicit: true, wantMerge: 1, wantPresence: "absent", wouldBlock: true, outcome: "landing-submitted"},
		{name: "pass", state: "success", summary: "pass for base base456: ready", wantMerge: 1, wantPresence: "present", outcome: "landing-submitted", baseMatch: "matched"},
		{name: "fail", state: "failure", summary: "fail for base base456: fix", wantPresence: "present", outcome: "existing-gate-refusal", baseMatch: "matched"},
		{name: "pending", state: "pending", summary: "defer for base base456: wait", wantPresence: "present", outcome: "existing-gate-refusal", baseMatch: "matched"},
		{name: "stale", state: "success", summary: "pass for base old: ready", wantPresence: "present", outcome: "existing-gate-refusal", baseMatch: "stale"},
		{name: "malformed", state: "success", summary: "no reviewed base", wantPresence: "present", outcome: "existing-gate-refusal", reason: "malformed-verdict", baseMatch: "unparseable"},
		{name: "opt-out", label: noMergeReviewLabel, wantPresence: "absent", outcome: "opted-out"},
		{name: "aborted", label: abortedRunLabel, wantPresence: "absent", outcome: "run-aborted"},
		{name: "advisory", advisory: true, wantPresence: "absent", outcome: "advisory"},
		{name: "draft", draft: true, wantPresence: "absent", outcome: "existing-gate-refusal"},
		{name: "null-shape", raw: `null`, wantMerge: 1, wantPresence: "unreadable", reason: "response-incomplete", outcome: "landing-submitted"},
		{name: "decode", raw: `{"statuses":`, phase: "checks", wantCode: 1, wantPresence: "unreadable", reason: "decode", outcome: "poll-error"},
		{name: "authentication", phase: "checks", wantCode: 1, wantPresence: "unreadable", reason: "authentication", outcome: "poll-error"},
		{name: "comments-error", phase: "comments", wantCode: 1, wantPresence: "unreadable", reason: "authentication", outcome: "poll-error"},
		{name: "commit-message-error", wantCode: 1, wantPresence: "absent", outcome: "commit-message-error"},
		{name: "premerge-error", wantCode: 1, wantPresence: "absent", outcome: "premerge-check-error"},
		{name: "policy-error", wantCode: 1, wantPresence: "absent", outcome: "policy-error"},
		{name: "annotation-failure", annotationFails: true, wantMerge: 1, wantPresence: "absent", wouldBlock: true, outcome: "landing-submitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearJournalPlaneEnv(t)
			st := &mergePRServerState{checkState: "success", headSHA: "head123", baseSHA: "base456", draft: tc.draft}
			if tc.state != "" {
				st.verdictStatus = map[string]interface{}{"context": providers.MergeReviewStatusContext, "state": tc.state, "description": tc.summary}
			}
			if tc.label != "" {
				st.labels = []string{tc.label}
			}
			upstream := newMergePRServer(t, "your-org", "your-repo", st)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/status") && (tc.raw != "" || tc.phase == "checks") {
					if tc.raw == "" {
						w.WriteHeader(http.StatusUnauthorized)
					}
					_, _ = fmt.Fprint(w, tc.raw)
					return
				}
				if tc.phase == "comments" && strings.HasSuffix(r.URL.Path, "/issues/9/comments") {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if tc.name == "policy-error" && strings.Contains(r.URL.Path, "/rules/branches/") {
					_, _ = fmt.Fprint(w, "malformed")
					return
				}
				upstream.Config.Handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			inputs := map[string]string{"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456"}
			if tc.name == "commit-message-error" {
				inputs["verdictAuthor"] = "different-author"
			}
			if tc.name == "premerge-error" {
				inputs["preMergeCheck"] = "go build ./..."
			}
			if tc.explicit {
				inputs["commitMessage"] = "Explicit standalone merge"
			}
			if tc.advisory {
				inputs["advisoryMode"] = "true"
			}
			root, dir := mergePREnv(t, server.URL, false, inputs)
			if tc.annotationFails {
				previous := openStageAnnotator
				openStageAnnotator = func(instance.Layout) (stageAnnotator, error) { return nil, errors.New("secret-sentinel") }
				t.Cleanup(func() { openStageAnnotator = previous })
			}
			code, stdout, stderr := runArgs(t, "merge-pr", root)
			if code != tc.wantCode || st.mergeCalls != tc.wantMerge {
				t.Fatalf("code=%d merges=%d stdout=%s stderr=%s", code, st.mergeCalls, stdout, stderr)
			}
			if strings.Contains(stderr, "secret-sentinel") {
				t.Fatal("annotation error leaked")
			}
			if tc.wantMerge == 1 && readMergeResult(t, dir)["merged"] != true {
				t.Fatal("missing merge receipt")
			}
			if tc.annotationFails {
				if !strings.Contains(stderr, "annotation unavailable (open)") {
					t.Fatal(stderr)
				}
				return
			}
			events := shadowEvents(t, root)
			if len(events) != 1 {
				t.Fatalf("annotations=%+v", events)
			}
			got := events[0].Runner
			if got["observation"] != tc.wantPresence || got["wouldBlock"] != tc.wouldBlock || got["outcome"] != tc.outcome || got["expectedBaseSHA"] != "base456" {
				t.Fatalf("annotation=%+v", got)
			}
			if tc.reason != "" && got["reasonClass"] != tc.reason {
				t.Fatalf("reason=%v want=%s", got["reasonClass"], tc.reason)
			}
			if tc.phase != "" && got["pollPhase"] != tc.phase {
				t.Fatalf("phase=%v", got["pollPhase"])
			}
			if tc.baseMatch != "" && got["baseMatch"] != tc.baseMatch {
				t.Fatalf("baseMatch=%v", got["baseMatch"])
			}
		})
	}
}

func TestMergeVerdictShadowADOIsUnsupported(t *testing.T) {
	clearJournalPlaneEnv(t)
	server, state := newADOMergePRServer(t, "headsha1", "basesha1")
	root, _ := adoMergePREnv(t, server.URL, false, map[string]string{"pullNumber": "359", "verdict": "pass", "headSha": "headsha1", "baseSha": "basesha1"})
	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 0 || atomic.LoadInt64(&state.patchCalls) != 1 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	events := shadowEvents(t, root)
	if len(events) != 1 || events[0].Runner["observation"] != "unsupported" || events[0].Runner["wouldBlock"] != false {
		t.Fatalf("events=%+v", events)
	}
}

func emitShadowFixture(root string, stderr io.Writer) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app", URL: "https://fixture-user:secret-sentinel@github.com/acme/app"}
	poll := providers.PullRequestPollResult{HeadSHA: "head", BaseSHA: "base", VerdictReadStatus: providers.VerdictReadComplete}
	o := prstatus.ObserveVerdict(providers.NewGitHubProvider("fixture"), poll, nil, "base")
	recordMergeVerdictShadow(root, repo, "9", "head", "base", poll, o, true, "landing-submitted", io.Discard, stderr)
}

func TestMergeVerdictShadowPodTransportAndPrivacy(t *testing.T) {
	daemon := newAnnotationPlane(t, "run-1", "alpha")
	stampJournalPlaneEnv(t, daemon.server.URL, daemon.token, "run-1", "alpha")
	root := t.TempDir()
	var stderr bytes.Buffer
	emitShadowFixture(root, &stderr)
	got := daemon.log.snapshot()
	if stderr.Len() != 0 || len(got) != 1 || got[0].Reason != "merge-verdict-shadow" || got[0].RunID != "run-1" {
		t.Fatalf("events=%+v stderr=%s", got, stderr.String())
	}
	if strings.Contains(fmt.Sprint(got), "secret-sentinel") || got[0].Runner["repository"] != "github|github.com||acme|app|" {
		t.Fatalf("unsafe identity: %+v", got[0].Runner)
	}
	if _, err := os.Stat(layoutFor(root).SchedulerDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pod wrote local scheduler: %v", err)
	}
}

func TestMergeVerdictShadowAppendWarningIsSafe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"secret-sentinel"}}`)
	}))
	defer server.Close()
	stampJournalPlaneEnv(t, server.URL+"/secret-sentinel", "fixture", "run-1", "alpha")
	var stderr bytes.Buffer
	emitShadowFixture(t.TempDir(), &stderr)
	if !strings.Contains(stderr.String(), "annotation unavailable (append)") || strings.Contains(stderr.String(), "secret-sentinel") {
		t.Fatal(stderr.String())
	}
}

type shadowRetainedAnnotator struct{ *journal.InstanceLog }

func (shadowRetainedAnnotator) Close() error { return nil }

func TestMergeVerdictShadowUsesDaemonAgeCompaction(t *testing.T) {
	clearJournalPlaneEnv(t)
	root := t.TempDir()
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	eventTime := now.Add(-48 * time.Hour)
	log, _, err := journal.OpenInstanceLog(layoutFor(root).SchedulerDir(), journal.WithClock(func() time.Time { return eventTime }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	previous := openStageAnnotator
	openStageAnnotator = func(instance.Layout) (stageAnnotator, error) { return shadowRetainedAnnotator{log}, nil }
	t.Cleanup(func() { openStageAnnotator = previous })
	// Grow beyond the age window, then run the exact daemon compaction seam
	// repeatedly. Annotations are ordinary journal events, not tick-skip records.
	for day := 0; day < 6; day++ {
		eventTime = now.Add(time.Duration(day) * 24 * time.Hour)
		for range 4 {
			emitShadowFixture(root, io.Discard)
		}
		if err := compactSchedulerRetention(context.Background(), instance.TelemetryRetentionConfig{Window: "12h"}, nil, log, nil, eventTime); err != nil {
			t.Fatal(err)
		}
		if got := shadowEvents(t, root); len(got) != 4 {
			t.Fatalf("day %d: age compaction retained %d observations", day, len(got))
		}
	}
}
