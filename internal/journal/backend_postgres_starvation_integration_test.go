//go:build cloudintegration

package journal_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// Reviewer-2's starvation probe: 30k 1-KiB events, a sibling emitting at 10/s,
// and a gate verdict with a 20s caller deadline. Bulk fixture seeding avoids
// spending the local/hosted test budget writing the historical prefix.
func TestPostgresLifecycleAppendCompletesUnderConcurrentAppends(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	handle, owner := createPostgresRun(t, backend)
	seedPostgresHistory(t, config, owner, 30000)
	stop := make(chan struct{})
	first := make(chan struct{})
	var ticks atomic.Int64
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, err := backend.AppendBatch(t.Context(), handle, owner, []journal.Event{{Type: journal.EventRunnerAnnotation, Name: "tick"}}); err == nil {
					if ticks.Add(1) == 1 {
						close(first)
					}
				}
			}
		}
	})
	defer func() { close(stop); wg.Wait() }()
	select {
	case <-first:
	case <-time.After(10 * time.Second):
		t.Fatal("sibling never appended")
	}
	before := ticks.Load()
	bounded, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	started := time.Now()
	_, err := backend.AppendBatch(bounded, handle, owner, []journal.Event{{Type: journal.EventGateEvaluated, Gate: "review"}})
	t.Logf("gate append: elapsed=%s sibling appends=%d err=%v", time.Since(started), ticks.Load()-before, err)
	if err != nil {
		t.Fatal(err)
	}
	if ticks.Load() <= before {
		t.Fatal("probe did not exercise concurrent appends")
	}
}

func TestPostgresConcurrentLifecycleTailMatchesFullReplay(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	handle, owner := createPostgresRun(t, backend)
	entered, resume := make(chan struct{}), make(chan struct{})
	var unblock, first sync.Once
	defer unblock.Do(func() { close(resume) })
	result := make(chan error, 1)
	go func() {
		_, err := backend.AppendIfAbsent(t.Context(), handle, owner, journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Target: journal.TargetAbort}, func(journal.Event) bool {
			first.Do(func() { close(entered); <-resume })
			return false
		})
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("predicate did not start")
	}
	tail := []journal.Event{
		{Type: journal.EventParallelStarted, Parallel: "fan"},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "left", Stage: "work"},
		{Type: journal.EventGatePaused, Gate: "review"},
		{Type: journal.EventStageStarted, Branch: 1, Stage: "next"},
	}
	if _, err := backend.AppendBatch(t.Context(), handle, owner, tail); err != nil {
		t.Fatal(err)
	}
	unblock.Do(func() { close(resume) })
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tail fold did not finish")
	}
	var raw []byte
	if err := config.Pool.QueryRow(t.Context(), `SELECT state FROM journal.run_checkpoint WHERE instance_id=$1 AND run_id=$2`, config.InstanceID, "run-guard").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got journal.State
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	reader, err := backend.OpenRun(t.Context(), journal.RunRef{ID: "run-guard"})
	if err != nil {
		t.Fatal(err)
	}
	want, err := reader.State()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checkpoint=%+v full replay=%+v", got, want)
	}
	if got.Phase != journal.PhaseRunning || len(got.Branches) != 1 || got.Branches[0].MachineState != "next" {
		t.Fatalf("tail not reflected: %+v", got)
	}
}
