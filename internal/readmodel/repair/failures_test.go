package repair

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

type failureWarning struct {
	RunID      string    `json:"run_id"`
	Error      string    `json:"error"`
	Count      uint64    `json:"failure_count"`
	Suppressed uint64    `json:"suppressed_failures"`
	Since      time.Time `json:"count_since"`
}

func readFailureWarnings(t *testing.T, logs *bytes.Buffer) []failureWarning {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(logs.Bytes()))
	var warnings []failureWarning
	for {
		var warning failureWarning
		if err := decoder.Decode(&warning); errors.Is(err, io.EOF) {
			return warnings
		} else if err != nil {
			t.Fatal(err)
		}
		warnings = append(warnings, warning)
	}
}

func TestRunFailureWarningsSummarizeCountsPerRun(t *testing.T) {
	var logs bytes.Buffer
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now := start
	s := New(nil, nil, nil, Options{Now: func() time.Time { return now }, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	err := errors.New("unreadable journal")
	s.recordRunFailure("run-a", "repair failed", err)
	s.recordRunFailure("run-a", "repair failed", err)
	now = start.Add(runFailureRepeatWindow - time.Nanosecond)
	s.recordRunFailure("run-a", "repair failed", err)
	s.recordRunFailure("run-b", "repair failed", err)
	if warnings := readFailureWarnings(t, &logs); len(warnings) != 2 || warnings[0].Count != 1 || warnings[1].RunID != "run-b" || warnings[1].Count != 1 {
		t.Fatalf("first failures were not independent or repeats were logged: %+v", warnings)
	}
	now = start.Add(runFailureRepeatWindow)
	s.recordRunFailure("run-a", "repair still failed", err)
	warnings := readFailureWarnings(t, &logs)
	if len(warnings) != 3 {
		t.Fatalf("warnings = %+v, want two initial warnings and one summary", warnings)
	}
	summary := warnings[2]
	if summary.RunID != "run-a" || summary.Count != 4 || summary.Suppressed != 2 || !summary.Since.Equal(start) {
		t.Fatalf("wrong repeat summary: %+v", summary)
	}
	if s.Stats().Failures != 5 {
		t.Fatalf("suppressed failures lost from aggregate: %+v", s.Stats())
	}
}

func TestRunFailureCountsAreBoundedAndEvictionIsExplicit(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	s := New(nil, nil, nil, Options{Now: func() time.Time { return now }, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	err := errors.New("unreadable journal")
	for i := 0; i < runFailureLimit; i++ {
		now = now.Add(time.Second)
		s.recordRunFailure(fmt.Sprintf("run-%d", i), "repair failed", err)
	}
	now = now.Add(time.Second)
	s.recordRunFailure("run-0", "repair failed", err)
	s.recordRunFailure("run-new", "repair failed", err)
	if len(s.failures) != runFailureLimit || s.failures["run-0"].count != 2 || s.failures["run-1"] != nil {
		t.Fatal("diagnostic cache did not retain recent failures within its fixed bound")
	}
	now = now.Add(time.Second)
	s.recordRunFailure("run-1", "repair failed", err)
	warnings := readFailureWarnings(t, &logs)
	last := warnings[len(warnings)-1]
	if len(s.failures) != runFailureLimit || last.RunID != "run-1" || last.Count != 1 || !last.Since.Equal(now) {
		t.Fatalf("evicted counter did not restart with an explicit new scope: %+v", last)
	}
	if s.Stats().Failures != runFailureLimit+3 {
		t.Fatalf("eviction changed aggregate count: %+v", s.Stats())
	}
}

type failingIdentityBackend struct {
	journal.ReaderBackend
	runID string
	err   error
}

func (b *failingIdentityBackend) OpenRun(ctx context.Context, ref journal.RunRef) (journal.RunReader, error) {
	reader, err := b.ReaderBackend.OpenRun(ctx, ref)
	if err == nil && ref.ID == b.runID && b.err != nil {
		return failingIdentityReader{RunReader: reader, err: b.err}, nil
	}
	return reader, err
}

type failingIdentityReader struct {
	journal.RunReader
	err error
}

func (r failingIdentityReader) Identity() (journal.RunIdentity, error) {
	return journal.RunIdentity{}, r.err
}

func TestBackendIdentityFailuresAreCountedAndRetried(t *testing.T) {
	store := openStore(t)
	root := t.TempDir()
	writeRun(t, root, "run-a-invalid")
	writeRun(t, root, "run-b-valid")
	backend := &failingIdentityBackend{
		ReaderBackend: journal.NewFileReaderBackend(journal.FileReaderBackendConfig{RunsDirs: []string{root}}),
		runID:         "run-a-invalid", err: errors.New("identity unreadable"),
	}
	var logs bytes.Buffer
	now := time.Now().Add(time.Hour)
	s := New(store, store, nil, Options{Journal: backend, BatchSize: 10,
		Now: func() time.Time { return now }, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	for step := 0; step < 2; step++ {
		if err := s.Step(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if s.Stats().Failures != 2 || s.Stats().Projected != 1 {
		t.Fatalf("identity failures were not counted while discovery progressed: %+v", s.Stats())
	}
	if _, found, err := store.GetRun(t.Context(), "run-b-valid"); err != nil || !found {
		t.Fatalf("healthy neighbour not projected: found=%v error=%v", found, err)
	}
	if _, found, err := store.GetRun(t.Context(), backend.runID); err != nil || found {
		t.Fatalf("unreadable identity was projected: found=%v error=%v", found, err)
	}
	warnings := readFailureWarnings(t, &logs)
	if len(warnings) != 1 || warnings[0].RunID != backend.runID || !strings.Contains(warnings[0].Error, "read identity") || !strings.Contains(warnings[0].Error, "identity unreadable") {
		t.Fatalf("identity error diagnostic missing or repeated: %+v", warnings)
	}
	now = now.Add(runFailureRepeatWindow)
	if err := s.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	warnings = readFailureWarnings(t, &logs)
	if len(warnings) != 2 || warnings[1].Count != 3 || warnings[1].Suppressed != 1 {
		t.Fatalf("identity failures not summarized: %+v", warnings)
	}
	backend.err = nil
	if err := s.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.GetRun(t.Context(), backend.runID); err != nil || !found {
		t.Fatalf("recovered identity not projected: found=%v error=%v", found, err)
	}
	if s.Stats().Failures != 3 {
		t.Fatalf("recovery added a failure: %+v", s.Stats())
	}
}

func TestReverseLookupFailuresShareSummaryAccounting(t *testing.T) {
	store := openStore(t)
	root := t.TempDir()
	writeRun(t, root, "run-unreadable")
	backend := &failingBackend{ReaderBackend: journal.NewFileReaderBackend(journal.FileReaderBackendConfig{RunsDirs: []string{root}})}
	var logs bytes.Buffer
	now := time.Now().Add(time.Hour)
	s := New(store, store, nil, Options{Journal: backend, BatchSize: 10,
		Now: func() time.Time { return now }, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err := s.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	backend.openErr = errors.New("journal unavailable")
	for step := 0; step < 3; step++ {
		if err := s.Step(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	warnings := readFailureWarnings(t, &logs)
	if s.Stats().Failures != 3 || len(warnings) != 1 {
		t.Fatalf("reverse failures lost or repeated warnings: stats=%+v warnings=%+v", s.Stats(), warnings)
	}
	now = now.Add(runFailureRepeatWindow)
	if err := s.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	warnings = readFailureWarnings(t, &logs)
	if len(warnings) != 2 || warnings[1].Count != 4 || warnings[1].Suppressed != 2 {
		t.Fatalf("reverse failure summary = %+v", warnings)
	}
}
