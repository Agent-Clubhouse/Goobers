package journal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func batchTestKey(event Event) string {
	key, _ := event.Runner["batch-key"].(string)
	return key
}

func batchTestEvent(key string) Event {
	return Event{Type: EventRunnerAnnotation, Runner: map[string]any{"batch-key": key}}
}

func TestBatchDeduplicationConcurrentRetriesAndRestart(t *testing.T) {
	log, err := Create(t.TempDir(), RunIdentity{RunID: "batch", Workflow: "test", WorkflowVersion: 1, StartedAt: time.Now().UTC()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	batch := []Event{batchTestEvent("a"), batchTestEvent("a"), batchTestEvent("b")}
	var workers sync.WaitGroup
	var total atomic.Int64
	for range 4 {
		workers.Go(func() {
			count, err := log.AppendBatchIfAbsent(context.Background(), batch, batchTestKey)
			if err != nil {
				t.Error(err)
			}
			total.Add(int64(count))
		})
	}
	workers.Wait()
	if total.Load() != 2 {
		t.Fatalf("concurrent retries appended %d observations", total.Load())
	}
	directory := log.Dir()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := Recover(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if count, err := reopened.AppendBatchIfAbsent(context.Background(), batch, batchTestKey); err != nil || count != 0 {
		t.Fatalf("restart forgot durable deduplication: %d %v", count, err)
	}
	if count, err := reopened.AppendBatchIfAbsent(context.Background(), []Event{batchTestEvent("c")}, batchTestKey); err != nil || count != 1 {
		t.Fatalf("restart suppressed new observation: %d %v", count, err)
	}
}

func TestBatchDeduplicationRefusesTornOrCorruptHistoryWithoutWriting(t *testing.T) {
	for _, suffix := range []string{"{\"type\":", "corrupt\n"} {
		log, err := Create(t.TempDir(), RunIdentity{RunID: "batch", Workflow: "test", WorkflowVersion: 1, StartedAt: time.Now().UTC()}, nil)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(log.Dir(), fileEvents)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before = append(before, suffix...)
		if err := os.WriteFile(path, before, 0o600); err != nil {
			t.Fatal(err)
		}
		count, err := log.AppendBatchIfAbsent(context.Background(), []Event{batchTestEvent("a")}, batchTestKey)
		if err == nil || count != 0 {
			t.Fatalf("invalid history acknowledged: %d %v", count, err)
		}
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
			t.Fatalf("refused batch changed evidence: %v", err)
		}
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBatchDeduplicationRejectsCancelledAndOversizedBatches(t *testing.T) {
	log, err := Create(t.TempDir(), RunIdentity{RunID: "batch", Workflow: "test", WorkflowVersion: 1, StartedAt: time.Now().UTC()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if count, err := log.AppendBatchIfAbsent(ctx, []Event{batchTestEvent("a")}, batchTestKey); !errors.Is(err, context.Canceled) || count != 0 {
		t.Fatalf("cancelled batch acknowledged: %d %v", count, err)
	}
	if count, err := log.AppendBatchIfAbsent(context.Background(), make([]Event, 129), batchTestKey); err == nil || count != 0 {
		t.Fatalf("oversized batch acknowledged: %d %v", count, err)
	}
	transition := batchTestEvent("finish")
	transition.Type = EventRunFinished
	if count, err := log.AppendBatchIfAbsent(context.Background(), []Event{transition}, batchTestKey); err == nil || count != 0 {
		t.Fatalf("lifecycle transition bypassed normal append: %d %v", count, err)
	}
}
