package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// Every daemon stage runs its providers with run attribution on, so every
// comment, issue and reply Goobers writes carries the attribution footer.
// Stage tests used to build providers with it off unless a test set the whole
// run context, which let code that reads back its own text and compares it
// exactly pass here and fail in every daemon run (FU1 and its siblings). The
// suite therefore attributes stage provider writes by default
// (defaultTestStageAttribution, armed by the init in testmain_test.go).

// testStageAttributionEnv opts a test out of the suite's default stage
// attribution; see withoutDefaultStageAttribution.
const testStageAttributionEnv = "GOOBERS_TEST_STAGE_ATTRIBUTION"

// defaultTestStageAttribution is the suite's stageAttributionFor. A complete
// run context in the env attributes exactly as production does. Otherwise it
// fills only the missing identity fields, for the attribution alone: the
// process env is never touched, so ledgers, cursors, no-op guards, telemetry
// and cost routing that key off GOOBERS_GAGGLE and friends are unchanged. The
// filled identity still goes through the real cost-publication gate, cost
// receipt and instance-identity logic (stageAttributionFrom); the gate's
// notice about a placeholder gaggle is not production output and is dropped.
func defaultTestStageAttribution(root string) (providers.Attribution, bool) {
	if attribution, ok := stageAttribution(root); ok {
		return attribution, true
	}
	if os.Getenv(testStageAttributionEnv) == "off" {
		return providers.Attribution{}, false
	}
	identity := stageAttributionEnvIdentity()
	for _, field := range []struct {
		value    *string
		fallback string
	}{
		{&identity.runID, "run-test-attribution"},
		{&identity.gaggle, "test-gaggle"},
		{&identity.workflow, "test-workflow"},
		{&identity.task, "test-task"},
		{&identity.goober, "deterministic"},
	} {
		if *field.value == "" {
			*field.value = field.fallback
		}
	}
	return stageAttributionFrom(root, identity, io.Discard), true
}

// withoutDefaultStageAttribution turns the suite's default stage attribution
// off for t, for a test that pins the attribution-off contract of a
// standalone invocation. It is env-based so re-exec'd stage subprocesses see
// it too, and t.Setenv already forbids t.Parallel. A complete run context in
// the env still attributes, exactly as in production.
func withoutDefaultStageAttribution(t *testing.T) {
	t.Helper()
	t.Setenv(testStageAttributionEnv, "off")
}

// fixtureOwnAttribution is the attribution the fakes stamp on a comment they
// seed under Goobers' own identity, so a fixture's "our earlier comment" is
// stored the way a daemon run stored it.
var fixtureOwnAttribution = providers.Attribution{
	Gaggle: "test-gaggle", Workflow: "test-workflow", Task: "test-task",
	Goober: "deterministic", Run: "run-fixture-attribution", Instance: "fixture",
}

// stampOwnFixtureBody returns body as an attributed provider write stores it.
// A body that already carries an attribution marker is returned unchanged, so
// a fixture can seed a specific (or deliberately malformed) marker itself.
func stampOwnFixtureBody(body, action string) string {
	if strings.Contains(body, "goobers:attribution") {
		return body
	}
	stamped, err := providers.StampAttribution(body, fixtureOwnAttribution, action)
	if err != nil {
		panic("stamp fixture attribution: " + err.Error())
	}
	return stamped
}

// assertBodyEqualIgnoringAttribution compares a body Goobers wrote with the
// text it meant to write, ignoring the attribution footer the provider adds.
// No test hardcodes the footer text.
func assertBodyEqualIgnoringAttribution(t *testing.T, got, want string) {
	t.Helper()
	if stripped := providers.StripAttribution(got); stripped != strings.TrimSpace(want) {
		t.Errorf("body without attribution = %q, want %q", stripped, strings.TrimSpace(want))
	}
}

// requireStageAttribution requires body to carry exactly one valid
// attribution marker naming task.
func requireStageAttribution(t *testing.T, body, task string) providers.Attribution {
	t.Helper()
	attribution, ok, err := providers.ParseAttribution(body)
	if err != nil || !ok {
		t.Fatalf("body %q has no valid attribution: ok=%v err=%v", body, ok, err)
	}
	if attribution.Task != task {
		t.Fatalf("attribution task = %q, want %q (body %q)", attribution.Task, task, body)
	}
	return attribution
}

// clearStageRunContext removes the run context a daemon injects, leaving a
// test's process env as a standalone invocation sees it.
func clearStageRunContext(t *testing.T) {
	t.Helper()
	for _, name := range []string{"GOOBERS_RUN_ID", "GOOBERS_GAGGLE", "GOOBERS_WORKFLOW", executor.TaskEnvVar, executor.GooberEnvVar} {
		t.Setenv(name, "")
	}
}

// TestStageTestsRunWithDaemonAttribution is the loud precondition for the
// suite's default: with no run context at all, a stage provider built through
// the production seam still stamps its writes. If the default is dropped from
// testmain_test.go or the seam is bypassed, this fails instead of the suite
// silently going back to attribution-off.
func TestStageTestsRunWithDaemonAttribution(t *testing.T) {
	clearStageRunContext(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "attribution precondition")
	prev := newGitHubProvider
	newGitHubProvider = server.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	provider, err := newProviderForStageAs[*providers.GitHubProvider](t.TempDir(), repo, false, withStageProviderToken("test-token"))
	if err != nil {
		t.Fatalf("build stage provider: %v", err)
	}
	comment, err := provider.CreateWorkItemComment(context.Background(), repo, "7", "precondition comment")
	if err != nil {
		t.Fatalf("post comment: %v", err)
	}
	stored, _ := fakeIssueComments(t, server, 7)
	if len(stored) != 1 {
		t.Fatalf("stored comments = %q, want one", stored)
	}
	for label, body := range map[string]string{"returned": comment.Body, "stored": stored[0]} {
		attribution := requireStageAttribution(t, body, "test-task")
		if attribution.Gaggle != "test-gaggle" || attribution.Run != "run-test-attribution" {
			t.Errorf("%s attribution = %+v, want the suite's default identity", label, attribution)
		}
		assertBodyEqualIgnoringAttribution(t, body, "precondition comment")
	}
}

// TestDefaultStageAttributionFillsOnlyMissingFields pins how the default
// merges with a partial run context, and that it leaves the env alone.
func TestDefaultStageAttributionFillsOnlyMissingFields(t *testing.T) {
	clearStageRunContext(t)
	t.Setenv(executor.TaskEnvVar, "resolve-review-threads")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	attribution, ok := stageAttributionFor(t.TempDir())
	if !ok {
		t.Fatal("default stage attribution is off")
	}
	want := providers.Attribution{
		Gaggle: "test-gaggle", Workflow: "pr-remediation", Task: "resolve-review-threads",
		Goober: "deterministic", Run: "run-test-attribution",
	}
	if attribution.Gaggle != want.Gaggle || attribution.Workflow != want.Workflow || attribution.Task != want.Task ||
		attribution.Goober != want.Goober || attribution.Run != want.Run {
		t.Errorf("attribution = %+v, want identity %+v", attribution, want)
	}
	if attribution.Cost != nil {
		t.Errorf("attribution cost = %+v; a placeholder gaggle must not pass the cost gate", attribution.Cost)
	}
	if got := os.Getenv("GOOBERS_GAGGLE"); got != "" {
		t.Errorf("GOOBERS_GAGGLE = %q; the default must not touch the process env", got)
	}
	if _, ok := stageAttribution(t.TempDir()); ok {
		t.Error("production stageAttribution attributed an incomplete run context")
	}
}

// TestWithoutDefaultStageAttributionRestoresStandaloneContract: the opt-out
// turns the fallback off, but a complete run context still attributes.
func TestWithoutDefaultStageAttributionRestoresStandaloneContract(t *testing.T) {
	clearStageRunContext(t)
	withoutDefaultStageAttribution(t)
	if attribution, ok := stageAttributionFor(t.TempDir()); ok {
		t.Fatalf("attribution = %+v with the default opted out, want none", attribution)
	}
	t.Setenv("GOOBERS_RUN_ID", "run-complete")
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	t.Setenv(executor.TaskEnvVar, "open-pr")
	attribution, ok := stageAttributionFor(t.TempDir())
	if !ok || attribution.Run != "run-complete" || attribution.Task != "open-pr" {
		t.Fatalf("attribution = %+v, %v; a complete run context must still attribute", attribution, ok)
	}
}
