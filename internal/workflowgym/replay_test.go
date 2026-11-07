package workflowgym

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
)

const replayTestProviderFixture = "../harness/testdata/workflowgym/provider.json"

func TestReplayHistoricalFixtureIsOfflineAndDeterministic(t *testing.T) {
	req := ReplayRequest{
		Task:                replayTestTask(t),
		ProviderFixturePath: filepath.FromSlash(replayTestProviderFixture),
		JournalDir:          replayTestJournal(t, recordedComment()),
	}
	first, err := Replay(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Replay(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(firstJSON, secondJSON) {
		t.Fatalf("replay manifests differ:\n%s\n---\n%s", firstJSON, secondJSON)
	}
	if first.Status != "success" || first.JournalDigest == "" || len(first.NondeterminismFindings) != 0 ||
		len(first.VirtualizationGaps) != 0 {
		t.Fatalf("replay = %+v, want stable success", first)
	}
	// The recorded session reads its issue and re-posts its recorded comment;
	// list-open-issues is in the fixture but never requested, so it is absent.
	want := []string{
		"deterministic artifact transcript.jsonl",
		"deterministic artifact completion.json",
		"virtualized provider-read GET /repos/acme/web/issues/7 get-issue",
		"virtualized comment POST /repos/acme/web/issues/7/comments comment-on-issue",
	}
	if got := effectSummaries(first.Effects); !slices.Equal(got, want) {
		t.Fatalf("effects = %q, want %q", got, want)
	}
}

func TestReplayOmitsExchangesTheReplayNeverRequested(t *testing.T) {
	manifest, err := Replay(context.Background(), ReplayRequest{
		Task:                replayTestTask(t),
		ProviderFixturePath: filepath.FromSlash(replayTestProviderFixture),
		JournalDir:          replayTestJournal(t),
		Act: func(_ context.Context, req harness.RunRequest, _ *harness.BenchmarkSandbox) error {
			entries, err := os.ReadDir(req.Workspace)
			if err != nil {
				return err
			}
			if len(entries) != 0 {
				t.Errorf("replay workspace is not clean: %v", entries)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, effect := range manifest.Effects {
		if effect.Disposition != apiv1.WorkflowGymEffectDeterministic {
			t.Fatalf("effects = %+v, want only replayed artifacts when no exchange was requested", manifest.Effects)
		}
	}
	if manifest.Status != "success" || len(manifest.Effects) != 2 {
		t.Fatalf("manifest = %+v, want success with two replayed artifacts", manifest)
	}
}

func TestReplayRecordedMutationWithoutRequestIsUnavailable(t *testing.T) {
	manifest, err := Replay(context.Background(), ReplayRequest{
		Task:                replayTestTask(t),
		ProviderFixturePath: filepath.FromSlash(replayTestProviderFixture),
		JournalDir: replayTestJournal(t, journal.Event{
			Type: journal.EventRefTouched, Stage: "candidate", Attempt: 1,
			ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "9"},
			Runner:      map[string]any{"operation": "merge"},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "failure" || len(manifest.VirtualizationGaps) != 1 {
		t.Fatalf("manifest = %+v, want one virtualization gap", manifest)
	}
	want := []string{
		"deterministic artifact transcript.jsonl",
		"deterministic artifact completion.json",
		"virtualized provider-read GET /repos/acme/web/issues/7 get-issue",
		"unavailable merge merge github pr#9",
	}
	if got := effectSummaries(manifest.Effects); !slices.Equal(got, want) {
		t.Fatalf("effects = %q, want %q", got, want)
	}
}

func TestReplayClassifiesUnavailableEffectWithoutExecutingIt(t *testing.T) {
	manifest, err := Replay(context.Background(), ReplayRequest{
		Task:                replayTestTask(t),
		ProviderFixturePath: filepath.FromSlash(replayTestProviderFixture),
		JournalDir:          replayTestJournal(t),
		Act: func(ctx context.Context, _ harness.RunRequest, sandbox *harness.BenchmarkSandbox) error {
			_, err := sandbox.Run(ctx, "live-command", "--must-not-execute")
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "failure" || len(manifest.VirtualizationGaps) != 1 {
		t.Fatalf("manifest = %+v, want one unavailable virtualization gap", manifest)
	}
	var unavailable bool
	for _, effect := range manifest.Effects {
		if effect.Disposition == apiv1.WorkflowGymEffectUnavailable &&
			effect.Effect == string(harness.BenchmarkEffectSubprocess) &&
			effect.Target == "live-command" {
			unavailable = true
		}
	}
	if !unavailable {
		t.Fatalf("effects = %+v, want unavailable subprocess", manifest.Effects)
	}
}

func replayTestTask(t *testing.T) apiv1.WorkflowGymTaskManifest {
	t.Helper()
	registry, err := Load(filepath.Join("testdata", "valid", "10-train.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return registry.Tasks[0]
}

func effectSummaries(effects []apiv1.WorkflowGymReplayEffect) []string {
	out := make([]string, 0, len(effects))
	for _, effect := range effects {
		summary := fmt.Sprintf("%s %s %s", effect.Disposition, effect.Effect, effect.Target)
		if effect.Disposition == apiv1.WorkflowGymEffectVirtualized {
			summary += " " + effect.Detail
		}
		out = append(out, summary)
	}
	return out
}

func recordedComment() journal.Event {
	return journal.Event{
		Type: journal.EventRefTouched, Stage: "candidate", Attempt: 1,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "issue", ID: "7"},
		Runner:      map[string]any{"operation": "comment"},
	}
}

func replayTestJournal(t *testing.T, recorded ...journal.Event) string {
	t.Helper()
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{
		RunID:           "0af7651916cd43dd8448eb211c80319c",
		Workflow:        "workflow-gym",
		WorkflowVersion: 1,
		Gaggle:          "test",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "task-alpha"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "candidate", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	completion, err := json.Marshal(apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "historical result"})
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []struct {
		name string
		data []byte
	}{
		{harness.BenchmarkTranscriptArtifact, []byte("{\"message\":\"offline\"}\n")},
		{harness.BenchmarkCompletionArtifact, completion},
	} {
		if _, err := run.RecordStageArtifact("candidate", 1, journal.AttemptPolicy, artifact.name, artifact.data); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range recorded {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "candidate", Attempt: 1, Status: "success",
	}); err != nil {
		t.Fatal(err)
	}
	return run.Dir()
}
