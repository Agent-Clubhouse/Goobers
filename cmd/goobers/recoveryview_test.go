package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

func TestTraceShowsEffectiveRecoveryDeadline(t *testing.T) {
	layout := instance.NewLayout(initScheduledDemo(t))
	start := time.Now().UTC().Add(-45 * 24 * time.Hour)
	const runID = "retained-trace"
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: runID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: start}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	archive := []byte("metadata fixture; restore must independently verify Git bundle")
	record := recovery.Record{Version: 1, RunID: runID, RepositoryKey: "github|||team|repo|", Ref: "refs/goobers/recovery/" + runID, BaseSHA: strings.Repeat("a", 40), SnapshotSHA: strings.Repeat("b", 40), PatchDigest: "sha256:" + strings.Repeat("c", 64), ArchiveDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(archive)), ArchiveBytes: int64(len(archive)), CreatedAt: start, RetainUntil: start.Add(time.Hour)}
	key := sha256.Sum256([]byte(record.RepositoryKey + "\x00" + record.RunID + "\x00" + record.SnapshotSHA))
	directory := filepath.Join(layout.Root, "recovery", fmt.Sprintf("%x", key))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, recovery.BundleFileName), archive, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, recovery.RecordFileName)
	if err := recovery.PublishRecord(path, record); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().UTC().Add(30 * 24 * time.Hour)
	if _, err := recovery.RenewRetention(context.Background(), path, deadline, 1024); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"--summary", "--json"} {
		var out, stderr bytes.Buffer
		if code := runTrace([]string{mode, runID, layout.Root}, &out, &stderr); code != 0 {
			t.Fatalf("trace %s failed: %d %s", mode, code, stderr.String())
		}
		for _, want := range []string{record.Ref, record.BaseSHA, record.PatchDigest, deadline.Format(time.RFC3339Nano)} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("trace %s omitted %s: %s", mode, want, out.String())
			}
		}
		if mode == "--json" {
			var got traceJSONResult
			if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Recovery == nil || len(got.Recovery.Snapshots) != 1 || got.Recovery.Snapshots[0].Expired {
				t.Fatalf("renewed recovery JSON = %+v %v", got.Recovery, err)
			}
		}
	}
	if view := runRecoveryView(context.Background(), layout, "different-run", time.Now()); view != nil {
		t.Fatal("recovery leaked across run identity")
	}
	if view := runRecoveryView(context.Background(), layout, runID, deadline); view == nil || !view.Snapshots[0].Expired {
		t.Fatal("deadline boundary did not report expired recovery")
	}
	runs := []runSummary{{RunID: runID}, {RunID: "different-run"}}
	summaries := statusRecoverySummaries(layout, runs, time.Now())
	if summaries[0].Recovery == nil || summaries[0].Recovery.Snapshots[0].Expired || summaries[1].Recovery != nil {
		t.Fatalf("status recovery summaries = %+v", summaries)
	}
	var statusText bytes.Buffer
	printStatusRecovery(&statusText, layout, runs, time.Now())
	if !strings.Contains(statusText.String(), record.Ref) || !strings.Contains(statusText.String(), deadline.Format(time.RFC3339Nano)) || strings.Contains(statusText.String(), "different-run") {
		t.Fatalf("incorrect status recovery text: %s", statusText.String())
	}
	assertStatusCommandRecovery(t, layout.Root, runID, record.Ref, deadline)
}

func assertStatusCommandRecovery(t *testing.T, root, runID, ref string, deadline time.Time) {
	t.Helper()
	code, stdout, stderr := runArgs(t, "status", "--json", root)
	if code != 0 {
		t.Fatalf("status recovery command failed: %d %s", code, stderr)
	}
	var result statusJSONOutput
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	for _, run := range result.Runs {
		if run.RunID == runID && run.Recovery != nil && len(run.Recovery.Snapshots) == 1 {
			snapshot := run.Recovery.Snapshots[0]
			if snapshot.Ref == ref && snapshot.RetainUntil.Equal(deadline) && !snapshot.Expired {
				return
			}
		}
	}
	t.Fatalf("status omitted effective recovery metadata: %s", stdout)
}

func TestRecoveryViewDistinguishesUnavailableFromAbsent(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if view := runRecoveryView(context.Background(), layout, "run", time.Now()); view != nil {
		t.Fatal("absent inventory reported as retained")
	}
	if err := os.MkdirAll(filepath.Join(layout.Root, "recovery", "incomplete"), 0o700); err != nil {
		t.Fatal(err)
	}
	view := runRecoveryView(context.Background(), layout, "run", time.Now())
	if view == nil || view.Status != "unavailable" || len(view.Snapshots) != 0 {
		t.Fatalf("partial inventory presented as absence: %+v", view)
	}
}
