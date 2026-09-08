package main

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

func setTestInstanceCostPublication(t *testing.T, root string, enabled bool) {
	t.Helper()
	path := instance.NewLayout(root).ConfigFile()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\ncost:\n  enabled: "+strconv.FormatBool(enabled)+"\n")...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCostPublicationADOControlsProviderCallsAndPreservesCloseOut(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			root := initDemo(t)
			t.Setenv(executor.GaggleEnvVar, "")
			setTestInstanceCostPublication(t, root, enabled)
			closer := &fakeADOWorkItemCloser{
				item:       providers.WorkItem{ID: "1456", State: "open"},
				prComments: []providers.Comment{costComment(t, "goobers", "implementation", "cost-run", 20, 8_000_000_000)},
			}
			var stdout, stderr bytes.Buffer
			errs := performPostMergeADOWithPRComments(context.Background(), closer, closer, backlogRef,
				providers.PullRequestPollResult{Number: 359, Body: "Fixes #1456"}, "359", root, backlogRef, &stdout, &stderr)
			if len(errs) != 0 {
				t.Fatalf("close-out: %v, %s", errs, &stderr)
			}
			if len(closer.statusReqs) != 1 || len(closer.commentReqs) != 1 {
				t.Fatalf("close-out missing: %+v", closer)
			}
			want := 0
			if enabled {
				want = 1
			}
			if closer.prCommentReads != want || len(closer.prCommentReqs) != want {
				t.Fatalf("cost PR reads/writes=%d/%d, want %d/%d", closer.prCommentReads, len(closer.prCommentReqs), want, want)
			}
			if strings.Contains(closer.commentReqs[0].Comment, "AIC") != enabled {
				t.Fatalf("unexpected cost disclosure: %s", closer.commentReqs[0].Comment)
			}
		})
	}
}

func TestCostPublicationGitHubImmediateAndDelayed(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run("delayed="+strconv.FormatBool(delayed)+"/enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
				st := newPostMergeServerState(20, "main", "Fixes #42", nil, nil)
				st.prComments = []string{costComment(t, "goobers", "implementation", "cost-run", 20, 8_000_000_000).Body}
				server := newPostMergeServer(t, "your-org", "your-repo", st)
				var root string
				command := "post-merge"
				if delayed {
					root = postMergeReconcileEnv(t, server.URL)
					command = "reconcile-post-merge"
				} else {
					root, _ = postMergeEnv(t, server.URL, false, map[string]string{"pullNumber": "20"})
				}
				t.Setenv(executor.GaggleEnvVar, "")
				setTestInstanceCostPublication(t, root, enabled)
				if delayed {
					if err := recordPostMergeTimeout(root, postMergeTestRepo(), "20", time.Now().Add(-time.Minute)); err != nil {
						t.Fatal(err)
					}
				}
				code, stdout, stderr := runArgs(t, command, root)
				if code != 0 {
					t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
				}
				st.mu.Lock()
				defer st.mu.Unlock()
				if st.issueState[42] != "closed" {
					t.Fatalf("issue not closed: %s", st.issueState[42])
				}
				summaries := 0
				for _, comment := range st.prComments {
					if strings.Contains(comment, postMergeCostSummaryMarker) {
						summaries++
					}
				}
				want := 0
				if enabled {
					want = 1
				}
				if summaries != want {
					t.Fatalf("PR cost summaries=%d, want %d; stderr=%s", summaries, want, stderr)
				}
				foundCloseOut := false
				for _, comment := range st.issueComments[42] {
					if strings.Contains(comment, "Merged in pull request #20.") {
						foundCloseOut = true
						if strings.Contains(comment, "AIC") != enabled {
							t.Fatalf("unexpected issue cost disclosure: %s", comment)
						}
					}
				}
				if !foundCloseOut {
					t.Fatal("missing normal issue close-out comment")
				}
			})
		}
	}
}
