package harness

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestBenchmarkHarnessReplayIsByteStable(t *testing.T) {
	workspace := t.TempDir()
	fixture := BenchmarkFixture{
		Schema:     BenchmarkFixtureSchema,
		Provider:   "github",
		Transcript: "{\"role\":\"assistant\",\"content\":\"done\"}\n",
		Metrics: map[string]float64{
			telemetry.AttrUsageCostUSD:           0.25,
			telemetry.AttrGenAIUsageOutputTokens: 7,
		},
		Operations: []BenchmarkOperation{{
			Kind: BenchmarkProviderRead, Target: "issues/8351",
			Response: json.RawMessage(`{"state":"open"}`),
		}},
		Artifacts: []BenchmarkArtifact{
			{Name: "result", MediaType: "application/json", Content: `{"status":"success"}`},
			{Name: "patch", MediaType: "text/x-patch", Content: "diff --git a/a b/a\n"},
		},
	}
	request := BenchmarkRequest{TaskID: "implement", Provider: "github", Workspace: workspace}

	first, err := (BenchmarkHarness{}).Replay(context.Background(), request, fixture)
	if err != nil {
		t.Fatal(err)
	}
	second, err := (BenchmarkHarness{}).Replay(context.Background(), request, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Journal) != string(second.Journal) {
		t.Fatalf("journal changed between replays:\n%s\n---\n%s", first.Journal, second.Journal)
	}
	if string(first.Outcome.Transcript) != fixture.Transcript ||
		first.Outcome.Metrics[telemetry.AttrUsageCostUSD] != 0.25 {
		t.Fatalf("outcome = %+v", first.Outcome)
	}
	if len(first.Artifacts) != 2 || first.Artifacts[0].Ref.Digest != journal.Digest(first.Artifacts[0].Data) {
		t.Fatalf("artifacts = %+v", first.Artifacts)
	}
	if len(first.Reads) != 1 || string(first.Reads[0]) != `{"state":"open"}` {
		t.Fatalf("reads = %s", first.Reads)
	}
	for i, event := range first.Events {
		if event.Seq != uint64(i+1) || event.Schema != journal.EventSchema {
			t.Fatalf("event %d = %+v", i, event)
		}
	}
}

func TestBenchmarkHarnessRejectsProviderVirtualizationGap(t *testing.T) {
	replay, err := (BenchmarkHarness{}).Replay(context.Background(), BenchmarkRequest{
		TaskID: "task", Provider: "ado", Workspace: t.TempDir(),
	}, BenchmarkFixture{Schema: BenchmarkFixtureSchema, Provider: "github"})
	if !errors.Is(err, ErrBenchmarkVirtualizationGap) {
		t.Fatalf("error = %v", err)
	}
	var replayErr *BenchmarkReplayError
	if !errors.As(err, &replayErr) || replayErr.Code != BenchmarkErrorVirtualizationGap {
		t.Fatalf("typed error = %#v", replayErr)
	}
	if !strings.Contains(string(replay.Journal), BenchmarkErrorVirtualizationGap) {
		t.Fatalf("journal does not report gap: %s", replay.Journal)
	}
}

func TestBenchmarkHarnessFailsClosedWithoutMutatingNetworkOrFilesystem(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}

	var connections atomic.Int32
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			connections.Add(1)
			_ = connection.Close()
		}
	}()

	for _, operation := range []BenchmarkOperation{
		{Kind: BenchmarkWorkspaceWrite, Target: outside},
		{Kind: BenchmarkCredentialRead, Target: "github-token"},
		{Kind: BenchmarkGitPush, Target: "origin"},
		{Kind: BenchmarkComment, Target: "http://" + listener.Addr().String() + "/issues/8351"},
		{Kind: BenchmarkMerge, Target: "pulls/1"},
	} {
		t.Run(string(operation.Kind), func(t *testing.T) {
			replay, replayErr := (BenchmarkHarness{}).Replay(context.Background(), BenchmarkRequest{
				TaskID: "task", Provider: "github", Workspace: workspace,
			}, BenchmarkFixture{
				Schema: BenchmarkFixtureSchema, Provider: "github",
				Operations: []BenchmarkOperation{operation},
			})
			if !errors.Is(replayErr, ErrBenchmarkMutationDenied) {
				t.Fatalf("error = %v", replayErr)
			}
			if !strings.Contains(string(replay.Journal), BenchmarkErrorMutationDenied) {
				t.Fatalf("journal does not report denial: %s", replay.Journal)
			}
		})
	}

	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "unchanged" {
		t.Fatalf("outside file changed to %q", data)
	}
	if connections.Load() != 0 {
		t.Fatalf("replay opened %d network connections", connections.Load())
	}
}

func TestBenchmarkHarnessVirtualSinksAreInMemoryOnly(t *testing.T) {
	workspace := t.TempDir()
	target := filepath.Join(workspace, "candidate.txt")
	fixture := BenchmarkFixture{
		Schema: BenchmarkFixtureSchema, Provider: "github",
		VirtualSinks: []BenchmarkVirtualSink{
			{Name: "workspace", Kind: BenchmarkWorkspaceWrite},
			{Name: "comments", Kind: BenchmarkComment},
		},
		Operations: []BenchmarkOperation{
			{Kind: BenchmarkWorkspaceWrite, Target: "candidate.txt", Sink: "workspace"},
			{Kind: BenchmarkComment, Target: "issues/8351", Sink: "comments"},
		},
	}
	replay, err := (BenchmarkHarness{}).Replay(context.Background(), BenchmarkRequest{
		TaskID: "task", Provider: "github", Workspace: workspace,
	}, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay.VirtualSinks) != 2 || replay.VirtualSinks[0].Target != "candidate.txt" {
		t.Fatalf("virtual receipts = %+v", replay.VirtualSinks)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("virtual workspace sink created a file: %v", err)
	}
}

func TestBenchmarkHarnessProjectsRecordedFailureMode(t *testing.T) {
	replay, err := (BenchmarkHarness{}).Replay(context.Background(), BenchmarkRequest{
		TaskID: "task", Provider: "github", Workspace: t.TempDir(),
	}, BenchmarkFixture{
		Schema: BenchmarkFixtureSchema, Provider: "github", FailureMode: "candidate timeout",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay.FailureMode != "candidate timeout" ||
		replay.Events[len(replay.Events)-1].Status != "failure" ||
		!strings.Contains(string(replay.Journal), BenchmarkErrorFixtureFailure) {
		t.Fatalf("failure replay = %+v\n%s", replay, replay.Journal)
	}
}

func TestDecodeBenchmarkFixtureIsStrict(t *testing.T) {
	_, err := DecodeBenchmarkFixture([]byte(`{"schema":"` + BenchmarkFixtureSchema + `","provider":"github","extra":true}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v", err)
	}
}
