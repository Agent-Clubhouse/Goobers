package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

func TestRecoveryCloseOutReadsOwnRunOverJournalPlane(t *testing.T) {
	const runID = "remote-closeout"
	record := seedCloseOutRecovery(t, t.TempDir(), runID)
	event, err := recovery.RetainedEvent(record)
	if err != nil {
		t.Fatal(err)
	}
	// Older event projections omit the redundant per-event run id. The
	// authenticated list envelope supplies that identity to the stage reader.
	event.RunID = ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer fixture" || !strings.HasSuffix(request.URL.Path, "/runs/"+runID+"/events") {
			t.Errorf("unexpected journal read: %s", request.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"runId": runID, "events": []journal.Event{event}})
	}))
	defer server.Close()
	setPlaneEnv(t, server.URL, "fixture", runID, "example")
	// No local journal or archive exists beneath this stage-side root.
	detail, err := issueCloseOutRecoveryDetail(t.TempDir(), runID)
	if err != nil || !strings.Contains(detail, record.Ref) || !strings.Contains(detail, record.RetainUntil.Format(time.RFC3339Nano)) {
		t.Fatalf("remote close-out recovery = %q %v", detail, err)
	}
}

func seedCloseOutRecovery(t *testing.T, root, runID string) recovery.Record {
	t.Helper()
	now := time.Now().UTC()
	record := recovery.Record{Version: 1, RunID: runID, RepositoryKey: "github|||your-org|your-repo|", Ref: "refs/goobers/recovery/" + runID, BaseSHA: strings.Repeat("a", 40), SnapshotSHA: strings.Repeat("b", 40), PatchDigest: "sha256:" + strings.Repeat("c", 64), ArchiveDigest: "sha256:" + strings.Repeat("d", 64), ArchiveBytes: 512, CreatedAt: now, RetainUntil: now.Add(30 * 24 * time.Hour)}
	log, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: now}, nil)
	if err != nil {
		t.Fatal(err)
	}
	event, err := recovery.RetainedEvent(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(event); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	return record
}
