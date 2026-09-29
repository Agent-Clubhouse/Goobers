package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

type startupTiming struct {
	StartedUTC, FinishedUTC time.Time
	StartupMS, ShutdownMS   float64
	Requests, Rejected      int64
	PostReady               *startupResponsiveness `json:",omitempty"`
	Health                  startupHealthAudit
}

type startupHealthCounters struct {
	Events, ShutdownEvents, AdmissionFailures, QueueDropped, ExportFailures uint64
	PrunedAge, PrunedBytes, MalformedFiles                                  uint64
}

type startupHealthAudit struct {
	Streams map[string]startupHealthCounters
}

func (a startupHealthAudit) complete(enabled bool, profile string) bool {
	if !enabled {
		return true
	}
	streams := []string{"diagnostics"}
	if profile != "health" {
		streams = append(streams, "journal", "traces")
	}
	for _, stream := range streams {
		if a.Streams[stream].ShutdownEvents != 1 {
			return false
		}
	}
	return true
}

func (a startupHealthAudit) clean(endpoint string) bool {
	for _, counters := range a.Streams {
		if counters.AdmissionFailures != 0 || counters.QueueDropped != 0 ||
			counters.PrunedAge != 0 || counters.PrunedBytes != 0 || counters.MalformedFiles != 0 {
			return false
		}
		if endpoint == "healthy" && counters.ExportFailures != 0 {
			return false
		}
	}
	return true
}

// Parse the daemon's independent health channel after exit. A warning can be
// emitted only at shutdown, after the startup timing sample has completed.
// Counters are cumulative per stream, so retain maxima rather than summing
// repeated snapshots.
func auditStartupHealth(path string) (startupHealthAudit, error) {
	file, err := os.Open(path)
	if err != nil {
		return startupHealthAudit{}, err
	}
	defer file.Close()
	audit := startupHealthAudit{Streams: make(map[string]startupHealthCounters)}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] != '{' || !strings.Contains(string(line), `"telemetry.export.health"`) {
			continue
		}
		var event struct {
			Event, Stream, Status                  string
			AdmissionFailures                      uint64
			PrunedAge, PrunedBytes, MalformedFiles uint64
			Queue                                  struct {
				Dropped, ExportFailures uint64
			}
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return audit, fmt.Errorf("decode startup health event: %w", err)
		}
		if event.Event != "telemetry.export.health" || event.Stream == "" {
			return audit, fmt.Errorf("invalid startup health event: %s", line)
		}
		counters := audit.Streams[event.Stream]
		counters.Events++
		if event.Status == "shutdown" {
			counters.ShutdownEvents++
		}
		counters.AdmissionFailures = max(counters.AdmissionFailures, event.AdmissionFailures)
		counters.QueueDropped = max(counters.QueueDropped, event.Queue.Dropped)
		counters.ExportFailures = max(counters.ExportFailures, event.Queue.ExportFailures)
		counters.PrunedAge = max(counters.PrunedAge, event.PrunedAge)
		counters.PrunedBytes = max(counters.PrunedBytes, event.PrunedBytes)
		counters.MalformedFiles = max(counters.MalformedFiles, event.MalformedFiles)
		audit.Streams[event.Stream] = counters
	}
	if err := scanner.Err(); err != nil {
		return audit, fmt.Errorf("scan startup daemon log: %w", err)
	}
	return audit, nil
}

// Startup probes need request/fault evidence, not a growing map of every seed
// identity ever received. Stream decoding to discard bounds receiver memory;
// the separate workflow scenarios keep their full reconciliation collector.
func consumeStartupReplay(w http.ResponseWriter, decoded io.Reader, responseMode int32) {
	if _, err := io.Copy(io.Discard, decoded); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if responseMode != 0 {
		rejects.Add(1)
		respondNetworkFault(w, responseMode)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func measureStartup(name, root, api string, waitForIndex bool, postReady time.Duration) startupTiming {
	log, err := os.Create(filepath.Join(out, name+"-daemon.log"))
	must(err)
	defer func() { must(log.Close()) }()
	c := cmd(context.Background(), "up", "--drain-timeout", "15s", root)
	c.Stdout, c.Stderr = log, log
	c.Env = append(c.Env, "GODEBUG=gctrace=1")
	rq, rejected := requests.Load(), rejects.Load()
	started := time.Now()
	result := startupTiming{StartedUTC: started.UTC()}
	must(c.Start())
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	exited := false
	defer func() {
		if !exited {
			_ = c.Process.Kill()
			<-done
		}
	}()
	for !startupReady(api) {
		select {
		case err := <-done:
			exited = true
			panic(fmt.Sprintf("startup child exited before readiness: %v", err))
		default:
		}
		if time.Since(started) >= time.Minute {
			panic("startup readiness deadline exceeded")
		}
		time.Sleep(50 * time.Millisecond)
	}
	result.StartupMS = float64(time.Since(started).Nanoseconds()) / 1e6
	if postReady > 0 {
		// Start immediately after observed readiness, before index waits or the
		// idle dwell can hide a replay burst. Baseline uses the same workload.
		result.PostReady = measureStartupResponsiveness(name, root, api, postReady)
	}
	if waitForIndex {
		waitStartupIndex(root)
	}
	// The default remains an idle lifecycle. Optional post-ready work above is
	// explicitly recorded and is not comparable to the original idle grid.
	// Both baseline and enabled retain the same final dwell.
	time.Sleep(time.Second)
	stopStarted := time.Now()
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	output, stopErr := cmd(stopCtx, "down", root).CombinedOutput()
	cancel()
	write(filepath.Join(out, name+"-down.log"), string(output))
	must(stopErr)
	select {
	case err := <-done:
		exited = true
		must(err)
	case <-time.After(20 * time.Second):
		panic("startup probe shutdown watchdog exceeded")
	}
	result.ShutdownMS = float64(time.Since(stopStarted).Nanoseconds()) / 1e6
	result.FinishedUTC = time.Now().UTC()
	result.Requests, result.Rejected = requests.Load()-rq, rejects.Load()-rejected
	result.Health, err = auditStartupHealth(filepath.Join(out, name+"-daemon.log"))
	must(err)
	return result
}

func startupReady(api string) bool {
	for _, path := range []string{"/readyz", "/api/v1/instance"} {
		response, err := client.Get(api + path)
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 {
			return false
		}
	}
	return true
}

func waitStartupIndex(root string) {
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		// Avoid the inspector's fallback directory scan before a manifest exists.
		if _, err := os.Stat(filepath.Join(spool(root), ".replay-index.db")); err == nil {
			if telemetry.InspectAzureReplayRoot(spool(root)).AccountingReady {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	panic("warm startup accounting deadline exceeded")
}

var startupSeedName = regexp.MustCompile(`^[0-9]{20}-seed\.ndjson$`)

// The only destructive action removes known synthetic seed files from a newly
// created, marked startup instance inside this invocation's output directory.
// Refuse symlinked path components and never recursively remove a directory.
func visitStartupSeeds(root string, remove bool) error {
	rel, err := filepath.Rel(out, root)
	if err != nil || filepath.Base(rel) != rel || !strings.HasPrefix(rel, "startup-") {
		return fmt.Errorf("startup fixture is outside the owned output directory")
	}
	marker, err := os.ReadFile(filepath.Join(root, ".startup-fixture"))
	if err != nil || string(marker) != "synthetic-startup-seeds-v1\n" {
		return fmt.Errorf("startup fixture ownership marker missing")
	}
	journal := filepath.Join(spool(root), "journal")
	for _, dir := range []string{root, filepath.Join(root, "telemetry-export"), spool(root), journal} {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("startup fixture path is not a real directory: %s", dir)
		}
	}
	entries, err := os.ReadDir(journal)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !startupSeedName.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("startup seed is not a regular file: %s", entry.Name())
		}
		if err := syncOrRemoveStartupSeed(filepath.Join(journal, entry.Name()), remove); err != nil {
			return err
		}
	}
	return nil
}

func syncOrRemoveStartupSeed(path string, remove bool) error {
	if remove {
		return os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
