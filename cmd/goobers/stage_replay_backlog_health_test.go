package main

import (
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/providers"
)

// backlog-health --feedback posts its re-curation evidence comment and then
// removes the ready label, in that order (updateRESTWorkItem). An attempt
// that fails between the two leaves the comment and the ready label, so the
// retry sees the same item at the same ready time and must find its own,
// attributed comment by marker rather than post a second one.
//
// The fixture runs that failed attempt in setup, with the label removal
// refused, and hands the harness the retry: run 1 must remove the label
// without commenting, run 2 must do nothing.

const (
	replayBacklogHealthIssue     = 7
	replayBacklogHealthRun       = "replay-backlog-health"
	replayBacklogHealthMarkerTag = "<!-- goobers:implementation-feedback ready-at="
)

// replayFaultTransport refuses the ready-label removal on the replay issue
// while fail is set, before the request reaches the fake. It answers 422, a
// status the provider does not retry, so the failed attempt ends at once.
type replayFaultTransport struct {
	fail  *atomic.Bool
	inner http.RoundTripper
}

func (f replayFaultTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.fail.Load() && r.Method == http.MethodDelete &&
		strings.HasSuffix(r.URL.Path, "/issues/"+strconv.Itoa(replayBacklogHealthIssue)+"/labels/"+providers.LabelReady) {
		return &http.Response{
			StatusCode: http.StatusUnprocessableEntity,
			Status:     "422 Unprocessable Entity",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"message":"label removal refused by the replay fixture"}`)),
			Request:    r,
		}, nil
	}
	return f.inner.RoundTrip(r)
}

func isImplementationFeedbackComment(body string) bool {
	return strings.Contains(body, replayBacklogHealthMarkerTag)
}

func replayIssueHasLabel(server *fakeGitHubServer, number int, label string) bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	return hasAllLabels(server.issues[number].labels, []string{label})
}

func replayBacklogHealthFeedbackGitHub(t *testing.T) replayFixture {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	now := time.Now().UTC()
	server.addIssue(replayBacklogHealthIssue, "Chronically failing", "goobers:approved", providers.LabelReady)
	server.setLabelEventTime(replayBacklogHealthIssue, providers.LabelReady, true, now.Add(-8*time.Hour))
	// A maintainer's comment is not Goobers' and must be left alone.
	server.addCommentAs(replayBacklogHealthIssue, "maintainer", "This keeps failing on the migration step.")
	writeImplementationOutcomeRun(t, root, "fail-7-a", "7", journal.PhaseFailed, now.Add(-7*time.Hour))
	writeImplementationOutcomeRun(t, root, "fail-7-b", "7", journal.PhaseEscalated, now.Add(-6*time.Hour))
	rebuildTelemetryQueryRollup(t, root)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", replayBacklogHealthRun)
	t.Setenv("GOOBERS_GAGGLE", "example")
	t.Setenv(providersnapshot.EnvVar, "replay-feedback-tick")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_IMPLEMENTATIONFAILURETHRESHOLD", "2")
	workDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(workDir, "implementation-feedback.json"))

	counter := &providerWriteCounter{}
	var failLabelRemoval atomic.Bool
	client := &http.Client{Transport: replayFaultTransport{fail: &failLabelRemoval, inner: http.DefaultTransport}}
	previous := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return server.newGitHubProvider(token, append(opts, providers.WithHTTPClient(replayCountingClient{counter: counter, inner: client}))...)
	}
	t.Cleanup(func() { newGitHubProvider = previous })

	// The failed attempt: the evidence comment lands, the label removal does not.
	failLabelRemoval.Store(true)
	code, stdout, stderr := runArgs(t, "backlog-health", "--feedback", root)
	failLabelRemoval.Store(false)
	if code == 0 {
		t.Fatalf("failed attempt: code = 0, want the refused label removal to fail it\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	if got := ownedGitHubComments(t, server, replayBacklogHealthIssue, isImplementationFeedbackComment); len(got) != 1 {
		t.Fatalf("failed attempt left %d feedback comments, want the one it posted before the label removal: %q", len(got), got)
	}
	if !replayIssueHasLabel(server, replayBacklogHealthIssue, providers.LabelReady) {
		t.Fatal("failed attempt removed the ready label; the retry has nothing to dedupe")
	}

	return replayFixture{
		run: func(t *testing.T) (int, string, string) {
			code, stdout, stderr := runArgs(t, "backlog-health", "--feedback", root)
			if code == 0 && replayIssueHasLabel(server, replayBacklogHealthIssue, providers.LabelReady) {
				return 1, stdout, "the ready label is still on the issue after the retry: " + stderr
			}
			return code, stdout, stderr
		},
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			return map[string][]string{
				"implementation feedback": ownedGitHubComments(t, server, replayBacklogHealthIssue, isImplementationFeedbackComment),
			}
		},
	}
}
