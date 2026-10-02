package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

func TestStartupReceiverDoesNotRetainPayloadIdentities(t *testing.T) {
	before := received.Load()
	for _, responseMode := range []int32{0, 1, 2} {
		w := httptest.NewRecorder()
		consumeStartupReplay(w, strings.NewReader(strings.Repeat("synthetic seed\n", 1024)), responseMode)
		want := http.StatusServiceUnavailable
		if responseMode == 0 {
			want = http.StatusOK
		}
		if w.Code != want {
			t.Fatalf("mode=%d status=%d want=%d", responseMode, w.Code, want)
		}
	}
	if received.Load() != before {
		t.Fatal("startup receiver used the reconciliation collector")
	}
	w := httptest.NewRecorder()
	consumeStartupReplay(w, startupBrokenReader{}, 0)
	if w.Code != http.StatusBadRequest {
		t.Fatal("truncated payload acknowledged")
	}
}

func TestStartupRecoveryReceiverReconcilesOnlyAcknowledgedRecords(t *testing.T) {
	startupRecoveryActive.Store(true)
	defer startupRecoveryActive.Store(false)
	id := strings.Repeat("a", 32)
	ids.Delete(id)
	defer ids.Delete(id)
	startupRecoveryDuplicates.Store(0)
	body := fmt.Sprintf(`{"data":{"baseData":{"properties":{"goobers.telemetry.record_id":%q}}}}`, id)
	w := httptest.NewRecorder()
	consumeStartupReplay(w, strings.NewReader(body+"\n"), 0)
	if w.Code != http.StatusOK {
		t.Fatalf("recovery status=%d", w.Code)
	}
	if _, ok := ids.Load(id); !ok {
		t.Fatal("acknowledged recovery identity was not retained")
	}
	w = httptest.NewRecorder()
	consumeStartupReplay(w, strings.NewReader(body+"\n"), 0)
	if w.Code != http.StatusOK || startupRecoveryDuplicates.Load() != 1 {
		t.Fatal("duplicate acknowledged recovery identity was not counted")
	}
	w = httptest.NewRecorder()
	consumeStartupReplay(w, strings.NewReader(`{"data":`), 0)
	if w.Code != http.StatusBadRequest {
		t.Fatal("malformed recovery payload was acknowledged")
	}
}

func TestStartupPendingRecordIDs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(spool(root), ".replay-bootstrap", "journal")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	path := filepath.Join(dir, "pending.ndjson")
	body := `{"schema":"goobers.dev/telemetry/azure-replay/v1"}` + "\n" +
		fmt.Sprintf(`{"data":{"baseData":{"properties":{"goobers.telemetry.record_id":%q}}}}`, id) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := startupPendingRecordIDs(root)
	if err != nil || len(got) != 1 || got[0] != id {
		t.Fatalf("pending IDs=%v error=%v", got, err)
	}
	if err := os.WriteFile(path, []byte(body+strings.Split(body, "\n")[1]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := startupPendingRecordIDs(root); err == nil {
		t.Fatal("duplicate pending identity passed")
	}
}

func TestStartupRecoveryReceiptAllowsNewShutdownRecords(t *testing.T) {
	previous := collectionProfile
	collectionProfile = "standard"
	defer func() { collectionProfile = previous }()
	r := &startupRecoveryReceipt{
		Expected: 2, Seen: 2, Requests: 1, AccountingReady: true,
		ShutdownMS: 100, AfterShutdownPendingRecords: 3,
		Health: startupHealthAudit{Streams: map[string]startupHealthCounters{
			"diagnostics": {ShutdownEvents: 1}, "journal": {ShutdownEvents: 1}, "traces": {ShutdownEvents: 1},
		}},
	}
	if !r.Successful() {
		t.Fatal("new records produced by the recovery shutdown invalidated prior replay delivery")
	}
	r.PendingRecords = 1
	if r.Successful() {
		t.Fatal("unreconciled records at delivery were accepted")
	}
}

func TestStartupHealthAuditCatchesShutdownLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	body := "startup complete\n" +
		`{"event":"telemetry.export.health","stream":"journal","admissionFailures":0,"queue":{"dropped":0,"exportFailures":0}}` + "\n" +
		"shutting down\n" +
		`{"event":"telemetry.export.health","stream":"journal","admissionFailures":8,"queue":{"dropped":0,"exportFailures":2}}` + "\n" +
		`{"event":"telemetry.export.health","stream":"diagnostics","admissionFailures":28,"queue":{"dropped":28,"exportFailures":2}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	audit, err := auditStartupHealth(path)
	if err != nil {
		t.Fatal(err)
	}
	if audit.clean("healthy") || audit.clean("stalled") {
		t.Fatalf("shutdown loss passed: %+v", audit)
	}
	if audit.complete(true, "standard") {
		t.Fatal("warning-only audit mistaken for complete shutdown counters")
	}
	if journal := audit.Streams["journal"]; journal.Events != 2 || journal.AdmissionFailures != 8 || journal.ExportFailures != 2 {
		t.Fatalf("cumulative journal counters were not retained: %+v", journal)
	}
	if diagnostics := audit.Streams["diagnostics"]; diagnostics.QueueDropped != 28 || diagnostics.AdmissionFailures != 28 {
		t.Fatalf("diagnostic queue loss was missed: %+v", diagnostics)
	}
	if err := os.WriteFile(path, []byte(`{"event":"telemetry.export.health","stream":"journal","queue":{"exportFailures":2}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	audit, err = auditStartupHealth(path)
	if err != nil || audit.clean("healthy") || !audit.clean("stalled") {
		t.Fatalf("stalled endpoint policy confused remote failure with local loss: %+v %v", audit, err)
	}
	if err := os.WriteFile(path, []byte(`{"event":"telemetry.export.health","stream":"journal","prunedAge":1,"prunedBytes":2,"malformedFiles":3}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	audit, err = auditStartupHealth(path)
	if err != nil || audit.clean("healthy") || audit.clean("stalled") {
		t.Fatalf("retention/corruption loss passed: %+v %v", audit, err)
	}
	if err := os.WriteFile(path, []byte(`{"event":"telemetry.export.health","stream":"journal"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := auditStartupHealth(path); err == nil {
		t.Fatal("malformed health event passed")
	}
	if err := os.WriteFile(path, []byte(
		`{"event":"telemetry.export.health","status":"shutdown","stream":"diagnostics"}`+"\n"+
			`{"event":"telemetry.export.health","status":"shutdown","stream":"journal"}`+"\n"+
			`{"event":"telemetry.export.health","status":"shutdown","stream":"traces"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	audit, err = auditStartupHealth(path)
	if err != nil || !audit.complete(true, "standard") || !audit.complete(true, "health") || !audit.clean("healthy") {
		t.Fatalf("complete zero-loss shutdown audit rejected: %+v %v", audit, err)
	}
}

func TestStartupHealthAuditRequiredStreamsByProfile(t *testing.T) {
	for _, tc := range []struct {
		profile string
		streams []string
	}{
		{profile: "health", streams: []string{"diagnostics"}},
		{profile: "journal", streams: []string{"diagnostics", "journal"}},
		{profile: "standard", streams: []string{"diagnostics", "journal", "traces"}},
		{profile: "diagnostic", streams: []string{"diagnostics", "journal", "traces"}},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			all := startupHealthAudit{Streams: map[string]startupHealthCounters{}}
			for _, stream := range tc.streams {
				all.Streams[stream] = startupHealthCounters{ShutdownEvents: 1}
			}
			if !all.complete(true, tc.profile) {
				t.Fatal("profile's required streams were rejected")
			}
			for _, stream := range tc.streams {
				incomplete := startupHealthAudit{Streams: map[string]startupHealthCounters{}}
				for key, counters := range all.Streams {
					if key != stream {
						incomplete.Streams[key] = counters
					}
				}
				if incomplete.complete(true, tc.profile) {
					t.Fatalf("missing %s shutdown was accepted", stream)
				}
			}
		})
	}
}

type startupBrokenReader struct{}

func (startupBrokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestStartupConfiguration(t *testing.T) {
	base := startupConfig{Rounds: 20, Prefill: "empty", Index: "cold", Endpoint: "healthy", Settle: time.Second}
	for _, name := range []string{"empty", "half", "near-cap", "cap", "tiny-files", "legacy"} {
		c := base
		c.Prefill = name
		if err := c.validate("startup"); err != nil {
			t.Fatal(err)
		}
	}
	for _, change := range []func(*startupConfig){
		func(c *startupConfig) { c.Rounds = 0 },
		func(c *startupConfig) { c.Rounds = 101 },
		func(c *startupConfig) { c.Prefill = "unknown" },
		func(c *startupConfig) { c.Index = "OS-cold" },
		func(c *startupConfig) { c.Endpoint = "external" },
		func(c *startupConfig) { c.Settle = -time.Second },
		func(c *startupConfig) { c.Settle = 61 * time.Second },
		func(c *startupConfig) { c.PostReady = -time.Second },
		func(c *startupConfig) { c.PostReady = 61 * time.Second },
		func(c *startupConfig) { c.VerifyDeferred = true },
	} {
		c := base
		change(&c)
		if c.validate("startup") == nil {
			t.Fatalf("invalid configuration accepted: %+v", c)
		}
	}
	deferred := base
	deferred.Prefill = "tiny-files"
	deferred.VerifyDeferred = true
	if err := deferred.validate("startup"); err != nil {
		t.Fatalf("valid deferred startup configuration rejected: %v", err)
	}
}

func TestStartupReadinessRequiresBothRoutes(t *testing.T) {
	for _, failing := range []string{"", "/readyz", "/api/v1/instance"} {
		t.Run(failing, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == failing {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			if got := startupReady(server.URL); got != (failing == "") {
				t.Fatalf("ready=%v failing=%q", got, failing)
			}
		})
	}
}

func TestStartupWarmRequiresCompleteAccounting(t *testing.T) {
	state := startupOccupancy{Manifest: true, Files: 2, Bytes: 100}
	stats := telemetry.AzureReplayStats{AccountingReady: true, PendingFiles: 2, PendingBytes: 100}
	if err := validateStartupWarm(state, stats); err != nil {
		t.Fatal(err)
	}
	for _, incomplete := range []telemetry.AzureReplayStats{
		{AccountingReady: true}, // Migrated schema is not reconciled accounting.
		{AccountingReady: false, PendingFiles: 2, PendingBytes: 100},
		{AccountingReady: true, PendingFiles: 1, PendingBytes: 100},
		{AccountingReady: true, PendingFiles: 2, PendingBytes: 99},
	} {
		if validateStartupWarm(state, incomplete) == nil {
			t.Fatalf("incomplete accounting accepted: %+v", incomplete)
		}
	}
	state.Manifest = false
	if validateStartupWarm(state, stats) == nil {
		t.Fatal("missing manifest accepted")
	}
}

func TestStartupSeedCleanupIsScoped(t *testing.T) {
	previous := out
	out = t.TempDir()
	t.Cleanup(func() { out = previous })
	root := filepath.Join(out, "startup-owned")
	seed := filepath.Join(spool(root), "journal", "00000000000000000000-seed.ndjson")
	other := filepath.Join(spool(root), "journal", "keep.ndjson")
	write(seed, "synthetic seed\n")
	write(other, "keep\n")
	if err := visitStartupSeeds(root, true); err == nil {
		t.Fatal("cleanup without marker succeeded")
	}
	write(filepath.Join(root, ".startup-fixture"), "synthetic-startup-seeds-v1\n")
	if err := visitStartupSeeds(root, false); err != nil {
		t.Fatal(err)
	}
	before, err := startupDiskState(root)
	if err != nil || before.Files != 2 || before.Bytes != 20 || before.Manifest {
		t.Fatalf("incorrect pre-cleanup occupancy: %+v %v", before, err)
	}
	if err := visitStartupSeeds(root, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(seed); !os.IsNotExist(err) {
		t.Fatalf("seed not removed: %v", err)
	}
	if body, err := os.ReadFile(other); err != nil || string(body) != "keep\n" {
		t.Fatalf("unrelated file changed: %q %v", body, err)
	}
	if err := visitStartupSeeds(filepath.Dir(out), true); err == nil {
		t.Fatal("outside output path accepted")
	}
}

func TestStartupSeedCleanupRejectsSymlink(t *testing.T) {
	previous := out
	out = t.TempDir()
	t.Cleanup(func() { out = previous })
	root := filepath.Join(out, "startup-owned")
	write(filepath.Join(root, ".startup-fixture"), "synthetic-startup-seeds-v1\n")
	journal := filepath.Join(spool(root), "journal")
	if err := os.MkdirAll(journal, 0700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "keep.ndjson")
	write(external, "untouched")
	if err := os.Symlink(external, filepath.Join(journal, "00000000000000000000-seed.ndjson")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := visitStartupSeeds(root, true); err == nil {
		t.Fatal("symlink seed accepted")
	}
	if _, err := startupDiskState(root); err == nil {
		t.Fatal("symlink occupancy accepted")
	}
	if body, err := os.ReadFile(external); err != nil || string(body) != "untouched" {
		t.Fatalf("external target changed: %q %v", body, err)
	}
}
