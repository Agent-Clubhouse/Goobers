package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// This explicitly invoked experiment brackets local runner preparation from
// StartTask entry through executor entry, including journal fsync/checkpoint and
// scratch-workspace preparation. Executor effects are stubbed. It is not daemon
// scheduling, process spawn, repository checkout or remote pod queue latency.
// Ten workflows start 100 runs over 30 minutes, producing 400 task samples.
// Select one enabled subcase and use -benchtime=1x plus an external watchdog.
func BenchmarkRunnerTelemetryDispatchNormalRate(b *testing.B) {
	if os.Getenv("GOOBERS_DISABLE_FSYNC") != "" {
		b.Fatal("dispatch measurement requires the fsync override to be absent")
	}
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprintf("enabled=%t", enabled), func(b *testing.B) {
			if b.N != 1 {
				b.Fatal("use -benchtime=1x for the fixed 30-minute experiment")
			}
			samples, elapsed := runDispatchProbeWorkload(b, enabled, 100, 30*time.Minute)
			if elapsed > 33*time.Minute {
				b.Errorf("could not sustain offered run rate: %s", elapsed)
			}
			raw := make([]int64, len(samples))
			for i, sample := range samples {
				raw[i] = sample.Nanoseconds()
			}
			b.Logf("dispatch samples_ns: %v", raw)
			sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
			p95, p99 := samples[(len(samples)-1)*95/100], samples[(len(samples)-1)*99/100]
			b.ReportMetric(float64(p95.Nanoseconds()), "dispatch-prepare-p95-ns")
			b.ReportMetric(float64(p99.Nanoseconds()), "dispatch-prepare-p99-ns")
			b.Logf("dispatch preparation: enabled=%t runs=100 samples=%d elapsed=%s p95=%s p99=%s", enabled, len(samples), elapsed, p95, p99)
		})
	}
}

func TestRunnerTelemetryDispatchProbeRealRunner(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			samples, _ := runDispatchProbeWorkload(t, enabled, 2, time.Millisecond)
			if len(samples) != 8 {
				t.Fatalf("samples=%d, want eight real runner dispatch boundaries", len(samples))
			}
		})
	}
}

func dispatchProbeMachines(t testing.TB) []*workflow.Machine {
	t.Helper()
	var machines []*workflow.Machine
	for i := range 10 {
		var tasks []apiv1.Task
		for stage := range 4 {
			next := workflow.TerminalComplete
			if stage < 3 {
				next = fmt.Sprintf("stage-%d", stage+1)
			}
			tasks = append(tasks, apiv1.Task{Name: fmt.Sprintf("stage-%d", stage), Type: apiv1.TaskDeterministic,
				Goal: "synthetic dispatch preparation", Run: &apiv1.DeterministicRun{Command: []string{"synthetic"}, Workspace: apiv1.WorkspaceScratch}, Next: next})
		}
		machine, err := workflow.Compile(workflow.Definition{Name: fmt.Sprintf("dispatch-%d", i), Version: 1,
			Spec: apiv1.WorkflowSpec{Gaggle: "synthetic", Start: "stage-0", Tasks: tasks}}, workflow.WithPreviewFeatures(true))
		if err != nil {
			t.Fatal(err)
		}
		machines = append(machines, machine)
	}
	return machines
}

func newDispatchProbeRunner(t testing.TB, root string, samples *dispatchProbeSamples, delegate SpanStarter) *Runner {
	t.Helper()
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{
		InstanceID: "dispatch-probe", RunsDir: filepath.Join(root, "runs"), ScratchDir: filepath.Join(root, "scratch"), Worktrees: manager,
		Telemetry: dispatchProbeSpans{delegate: delegate},
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return dispatchProbeExecutor{samples: samples}, nil
		},
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return "", errors.New("dispatch fixture must not clone a repository")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func runDispatchProbeWorkload(t testing.TB, enabled bool, count int, window time.Duration) ([]time.Duration, time.Duration) {
	t.Helper()
	root := t.TempDir()
	receiver := &dispatchProbeReceiver{keys: make(map[string]string)}
	server := httptest.NewServer(receiver)
	defer server.Close()
	var client *telemetry.Client
	var delegate SpanStarter
	if enabled {
		var err error
		client, err = telemetry.New(context.Background(), telemetry.Config{
			AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + server.URL,
			AzureMonitorHTTPClient:       server.Client(), AzureMonitorTraces: true, AzureMonitorJournalLogs: true, JournalLogs: true,
			JournalRoot: root, JournalInstanceID: "dispatch-probe", AzureMonitorReplayRoot: filepath.Join(root, "replay"),
			AzureMonitorReplayMaxAge: time.Hour, AzureMonitorReplayMaxBytes: 512 << 20,
		})
		if err != nil {
			t.Fatal(err)
		}
		delegate = client
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := client.Shutdown(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	samples := &dispatchProbeSamples{limit: count * 4, values: make([]time.Duration, 0, count*4)}
	r := newDispatchProbeRunner(t, root, samples, delegate)
	machines := dispatchProbeMachines(t)
	elapsed := dispatchProbeProduce(t, r, machines, count, window)
	if len(samples.values) != count*4 {
		t.Fatalf("dispatch observations=%d, want %d", len(samples.values), count*4)
	}
	expected := dispatchProbeSourceKeys(t, filepath.Join(root, "runs"), count)
	if enabled {
		dispatchProbeReconcile(t, client, receiver, expected, filepath.Join(root, "replay"))
	}
	return samples.values, elapsed
}

func dispatchProbeProduce(t testing.TB, r *Runner, machines []*workflow.Machine, count int, window time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	failures := make(chan error, len(machines))
	var wg sync.WaitGroup
	for worker, machine := range machines {
		wg.Go(func() {
			for i := worker; i < count; i += len(machines) {
				timer := time.NewTimer(time.Until(start.Add(time.Duration(i) * window / time.Duration(count))))
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				result, err := r.Start(ctx, StartInput{RunID: fmt.Sprintf("%032x", i+1), Machine: machine,
					Gaggle: "synthetic", Trigger: journal.Trigger{Kind: journal.TriggerManual}})
				if err != nil || result.Phase != journal.PhaseCompleted {
					if err != nil {
						failures <- fmt.Errorf("run %d: %w", i, err)
					} else {
						failures <- fmt.Errorf("run %d: phase=%s: %s", i, result.Phase, result.FailureMessage)
					}
					cancel()
					return
				}
			}
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
	timer := time.NewTimer(time.Until(start.Add(window)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-timer.C:
	}
	return time.Since(start)
}

func dispatchProbeReconcile(t testing.TB, client *telemetry.Client, receiver *dispatchProbeReceiver, expected []string, spool string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	for {
		missing, duplicates, invalid := receiver.missing(expected)
		if invalid || duplicates != 0 {
			t.Fatalf("invalid receiver=%t duplicates=%d", invalid, duplicates)
		}
		if missing == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("missing %d of %d durable source events", missing, len(expected))
		case <-time.After(100 * time.Millisecond):
		}
	}
	var stats telemetry.AzureReplayStats
	for {
		stats = telemetry.InspectAzureReplayRoot(spool)
		if stats.QueueDropped != 0 || stats.PrunedAge != 0 || stats.PrunedBytes != 0 {
			t.Fatalf("unexpected replay loss: %+v", stats)
		}
		if stats.AccountingReady {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("replay accounting did not become ready after delivery: %+v", stats)
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Logf("dispatch source reconciliation: expected=%d missing=0 duplicates=0 queue_dropped=%d pruned_age=%d pruned_bytes=%d admission_failures=%d export_failures=%d", len(expected), stats.QueueDropped, stats.PrunedAge, stats.PrunedBytes, stats.AdmissionFailures, stats.ExportFailures)
}
