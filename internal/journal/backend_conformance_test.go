package journal_test

import (
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestJournalBackendConformance(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		testJournalBackend(t, journal.NewFileBackend())
	})
}

func testJournalBackend(t *testing.T, backend journal.Backend) {
	t.Helper()
	root := t.TempDir()
	runsDir := filepath.Join(root, "runs")
	id := journal.RunIdentity{RunID: "run-backend", Gaggle: "g", Workflow: "implementation"}
	run, owner, err := backend.CreateRun(runsDir, id, map[string][]byte{"input": []byte("pinned")})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.AppendBatch(run, owner, []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implementation", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implementation", Attempt: 1, Status: "success"},
	}); err != nil {
		t.Fatal(err)
	}
	appended, err := backend.AppendIfAbsent(run, owner,
		journal.Event{Type: journal.EventRunnerAnnotation, Name: "once"},
		func(event journal.Event) bool {
			return event.Type == journal.EventRunnerAnnotation && event.Name == "once"
		},
	)
	if err != nil || !appended {
		t.Fatalf("append if absent = %v, %v", appended, err)
	}
	appended, err = backend.AppendIfAbsent(run, owner,
		journal.Event{Type: journal.EventRunnerAnnotation, Name: "once"},
		func(event journal.Event) bool {
			return event.Type == journal.EventRunnerAnnotation && event.Name == "once"
		},
	)
	if err != nil || appended {
		t.Fatalf("duplicate append = %v, %v", appended, err)
	}
	receipt := apiv1.NotificationReceipt{
		Schema: apiv1.NotificationReceiptSchema, NotificationID: "notice-1",
		IdempotencyKey: "notice-1", IdempotencyDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Sink: apiv1.NotificationSinkRef{Kind: "terminal", Version: "v1"}, Attempt: 1,
		Status: apiv1.NotificationPending,
	}
	if claimed, err := backend.ClaimNotificationDelivery(run, owner, receipt); err != nil || claimed != nil {
		t.Fatalf("notification claim = %+v, %v", claimed, err)
	}
	if claimed, err := backend.ClaimNotificationDelivery(run, owner, receipt); err != nil || claimed == nil {
		t.Fatalf("notification replay = %+v, %v", claimed, err)
	}
	run.SetMachineState("implementation")
	if err := backend.Checkpoint(run, owner); err != nil {
		t.Fatal(err)
	}
	ref, err := backend.RecordBlob(run, owner, journal.BlobRecord{
		Event: journal.Event{Type: journal.EventArtifactRecorded, Name: "result.json", Integrity: apiv1.IntegrityDerived},
		Data:  []byte(`{"ok":true}`),
	})
	if err != nil || ref.Digest != journal.Digest([]byte(`{"ok":true}`)) {
		t.Fatalf("record blob = %+v, %v", ref, err)
	}
	if err := backend.Retire(run, owner); err != nil {
		t.Fatal(err)
	}
	if err := backend.AppendBatch(run, owner, []journal.Event{{Type: journal.EventRunnerAnnotation}}); err == nil {
		t.Fatal("retired owner token still wrote")
	}

	recovered, recoveredOwner, report, err := backend.RecoverRun(filepath.Join(runsDir, id.RunID))
	if err != nil || len(report.Events) < 5 {
		t.Fatalf("recover = events:%d, %v", len(report.Events), err)
	}
	if err := backend.AppendBatch(recovered, owner, []journal.Event{{Type: journal.EventRunnerAnnotation}}); err == nil {
		t.Fatal("prior owner token wrote through a reopened handle")
	}
	if err := backend.Retire(recovered, recoveredOwner); err != nil {
		t.Fatal(err)
	}

	instance, _, err := backend.OpenInstance(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.InstanceAppend(instance, journal.Event{Type: journal.EventDaemonStarted}); err != nil {
		t.Fatal(err)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	events, err := journal.ReadInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil || len(events) != 1 || events[0].Seq != 1 {
		t.Fatalf("instance events = %+v, %v", events, err)
	}

	if _, _, _, err := backend.RecoverRun(filepath.Join(root, "missing")); err == nil {
		t.Fatalf("missing recovery = %v", err)
	}
}
