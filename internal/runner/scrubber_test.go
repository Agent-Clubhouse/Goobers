package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// leakyDeterministic simulates an executor that resolves a credential (and
// correctly registers it, per #14's contract — see the test's
// NewDeterministic closure), but then accidentally echoes the raw value into
// an error message — the exact scenario #66 is about: a runner-authored
// event (executor_error) that isn't scrubbed by the executor's own
// pre-write scrubbing, only by whatever scrubber the run's own journal.Create
// was given.
type leakyDeterministic struct {
	token string
}

func (l *leakyDeterministic) Run(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{}, fmt.Errorf("upstream call failed: token=%s", l.token)
}

// TestRunnerJournalRedactsRegisteredSecretValue is #66's acceptance test: a
// real issued token, registered with the SecretRegistrar threaded to
// NewDeterministic, must be absent from the run's journal at rest — not just
// caught by the pattern net — even when it leaks into a field the runner
// itself writes (an executor_error message), not an artifact the executor
// scrubs and commits on its own.
//
// The token is deliberately opaque — no ghp_/AKIA/xox.../bearer-shaped
// prefix — so this test can only pass via the registry-value redaction #66
// wires in, never via journal's pattern-net fallback (a recognizable-shaped
// token would pass on unfixed code too, giving false assurance; see the
// negative control below).
func TestRunnerJournalRedactsRegisteredSecretValue(t *testing.T) {
	const token = "Kf9wQ2mNpZ7-internal-issued-value-with-no-known-shape"

	if scrubbed := journal.NewPatternScrubber().Scrub([]byte(token)); string(scrubbed) != token {
		t.Fatalf("negative control failed: the pattern net alone already redacts this token (%q) — it can't isolate the registry fix", token)
	}

	machine := fixtureMachine(t)
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)

	r, err := New(Config{
		NewDeterministic: func(_ ArtifactRecorder, reg SecretRegistrar) (invoke.Deterministic, error) {
			// Mirrors what a real executor does: register the token with
			// this run's registrar the moment it's resolved (#14's
			// contract), independent of whatever it later does — correctly
			// or not — with the value itself.
			reg.Register([]byte(token))
			return &leakyDeterministic{token: token}, nil
		},
		Worktrees: wtMgr,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The run fails (the executor errored) — that's expected and not what
	// this test is about; what matters is what actually landed in the
	// journal before it failed.
	if _, err := r.Start(context.Background(), StartInput{
		RunID:   "run-secret",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	}); err == nil {
		t.Fatal("Start: want an error from the leaky executor, got nil")
	}

	// Scan the raw on-disk journal — not just the typed Events() view — so a
	// leak into a field this test didn't anticipate would still be caught.
	runDir := filepath.Join(runsDir, "run-secret")
	err = filepath.WalkDir(runDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rErr := os.ReadFile(path)
		if rErr != nil {
			return rErr
		}
		if bytes.Contains(b, []byte(token)) {
			t.Errorf("file %s contains the raw issued token", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", runDir, err)
	}

	// Positive control: confirm the scrubber actually ran (redacted
	// placeholder present), not that the message was simply never written.
	eventsPath := filepath.Join(runDir, "events.jsonl")
	b, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	if !bytes.Contains(b, []byte("executor_error")) {
		t.Fatalf("events.jsonl missing the expected executor_error event: %s", b)
	}
	if !bytes.Contains(b, []byte(journal.Redacted)) {
		t.Fatalf("events.jsonl does not contain the redaction placeholder — scrubber may not have run: %s", b)
	}
}
