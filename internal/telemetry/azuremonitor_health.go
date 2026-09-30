package telemetry

// This last-resort health channel must never use the journal, slog's configured
// handler, an OTEL exporter, or the replay queue it observes. It contains only
// fixed causes and aggregate counts, never records, identities or error text.
import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

const replayHealthInterval = 10 * time.Second
const replayHealthRepeat = time.Minute
const replayHealthFileLimit = 1 << 20

type replayLossCounters struct {
	Dropped         uint64 `json:"dropped"`
	ExportFailures  uint64 `json:"exportFailures"`
	CatchupDeferred uint64 `json:"catchupDeferred"`
}
type replayLossSource struct{ sample func() replayLossCounters }
type replayHealthEvent struct {
	Time               time.Time          `json:"time"`
	Event              string             `json:"event"`
	Status             string             `json:"status"`
	Stream             string             `json:"stream"`
	PID                int                `json:"pid"`
	Causes             []string           `json:"causes"`
	AccountingReady    bool               `json:"accountingReady"`
	PendingRecords     int                `json:"pendingRecords"`
	PendingFiles       int                `json:"pendingFiles"`
	PendingBytes       int64              `json:"pendingBytes"`
	OldestSeconds      float64            `json:"oldestSeconds"`
	AdmittedPerSecond  float64            `json:"admittedPerSecond"`
	DeliveredPerSecond float64            `json:"deliveredPerSecond"`
	AdmissionFailures  uint64             `json:"admissionFailures"`
	Retried            uint64             `json:"retried"`
	PrunedAge          uint64             `json:"prunedAge"`
	PrunedBytes        uint64             `json:"prunedBytes"`
	Malformed          uint64             `json:"malformedFiles"`
	Queue              replayLossCounters `json:"queue"`
}

type replayHealthState struct {
	previous         AzureReplayStats
	at, lastWarning  time.Time
	growing          int
	warning          bool
	reportedLoss     uint64
	reportedDeferred uint64
	problemDelivered uint64
}

func (h *replayHealthState) sample(now time.Time, stats AzureReplayStats, loss replayLossCounters, capBytes int64) *replayHealthEvent {
	var causes []string
	if !stats.AccountingReady {
		causes = append(causes, "accounting_unavailable")
	}
	if stats.PendingBytes >= capBytes-capBytes/5 && stats.PendingBytes > 0 {
		causes = append(causes, "spool_high_water")
	}
	if stats.OldestPendingAge >= 30*time.Second {
		causes = append(causes, "backlog_old")
	}
	// Successive counts can rise even when each sample sees a different fresh
	// batch. Count sustained growth only if backlog survives the actual sample
	// interval; stale accounting cannot establish that continuity either.
	elapsed := now.Sub(h.at)
	if !h.at.IsZero() && elapsed > 0 && stats.AccountingReady && h.previous.AccountingReady &&
		stats.PendingRecords > h.previous.PendingRecords && stats.OldestPendingAge >= elapsed {
		h.growing++
	} else {
		h.growing = 0
	}
	if h.growing >= 2 {
		causes = append(causes, "ingress_exceeds_delivery")
	}
	// These counters overlap (e.g. a failed admission is also an export failure).
	// Their sum is only a change detector, never reported as a loss total.
	lossMarker := stats.AdmissionFailures + stats.PrunedAge + stats.PrunedBytes + stats.Malformed + loss.Dropped + loss.ExportFailures
	if lossMarker > h.reportedLoss {
		causes = append(causes, "loss_or_export_failure")
	}
	if loss.CatchupDeferred > h.reportedDeferred {
		causes = append(causes, "journal_catchup_deferred")
	}
	if len(causes) > 0 {
		// Record this even when the warning is rate-limited: delivery before
		// the latest observed problem cannot prove that problem has cleared.
		h.problemDelivered = stats.Delivered
	} else if h.warning && stats.Delivered <= h.problemDelivered {
		// A quiet stream may have no pending spool records because admission
		// failed. Silence alone does not prove that storage or upload works.
		causes = append(causes, "recovery_unconfirmed")
	}
	event := &replayHealthEvent{Time: now.UTC(), Event: "telemetry.export.health", Status: "warning", Causes: causes,
		AccountingReady: stats.AccountingReady,
		PendingRecords:  stats.PendingRecords, PendingFiles: stats.PendingFiles, PendingBytes: stats.PendingBytes,
		OldestSeconds: stats.OldestPendingAge.Seconds(), AdmissionFailures: stats.AdmissionFailures, Retried: stats.Retried,
		PrunedAge: stats.PrunedAge, PrunedBytes: stats.PrunedBytes, Malformed: stats.Malformed, Queue: loss}
	if seconds := elapsed.Seconds(); !h.at.IsZero() && seconds > 0 {
		event.AdmittedPerSecond = float64(stats.Accepted-h.previous.Accepted) / seconds
		event.DeliveredPerSecond = float64(stats.Delivered-h.previous.Delivered) / seconds
	}
	h.previous, h.at = stats, now
	if len(causes) == 0 {
		if !h.warning {
			return nil
		}
		h.warning = false
		event.Status = "recovered"
		return event
	}
	if !h.lastWarning.IsZero() && now.Sub(h.lastWarning) < replayHealthRepeat {
		return nil
	}
	h.lastWarning = now
	h.warning = true
	h.reportedLoss = lossMarker
	h.reportedDeferred = loss.CatchupDeferred
	return event
}

func (s *azureReplaySpool) runHealth(ctx context.Context) {
	defer close(s.healthDone)
	ticker := time.NewTicker(replayHealthInterval)
	defer ticker.Stop()
	state := replayHealthState{}
	for {
		select {
		case <-ctx.Done():
			s.reportHealth(&state)
			return
		case <-ticker.C:
			s.reportHealth(&state)
		}
	}
}

func (s *azureReplaySpool) reportHealth(state *replayHealthState) {
	loss := replayLossCounters{}
	if source := s.lossSource.Load(); source != nil {
		loss = source.sample()
	}
	event := state.sample(time.Now(), s.stats(), loss, s.cfg.maxBytes)
	if event == nil {
		return
	}
	event.Stream = s.stream
	if event.Stream == "" {
		event.Stream = "export"
	}
	event.PID = os.Getpid()
	root := s.cfg.root
	if root == "" {
		root = s.cfg.dir
	}
	writeReplayHealth(root, *event)
}

// A final snapshot makes shutdown accounting explicit even when the stream
// never warranted a warning. It is emitted after the final drain attempt, so
// release validation need not mistake health-log silence for zero loss.
func (s *azureReplaySpool) reportShutdownHealth(stats AzureReplayStats) {
	loss := replayLossCounters{Dropped: stats.QueueDropped, ExportFailures: stats.ExportFailures}
	if source := s.lossSource.Load(); source != nil {
		loss.CatchupDeferred = source.sample().CatchupDeferred
	}
	stream := s.stream
	if stream == "" {
		stream = "export"
	}
	root := s.cfg.root
	if root == "" {
		root = s.cfg.dir
	}
	writeReplayHealth(root, replayHealthEvent{
		Time: time.Now().UTC(), Event: "telemetry.export.health", Status: "shutdown",
		Stream: stream, PID: os.Getpid(), AccountingReady: stats.AccountingReady, PendingRecords: stats.PendingRecords,
		PendingFiles: stats.PendingFiles, PendingBytes: stats.PendingBytes,
		OldestSeconds: stats.OldestPendingAge.Seconds(), AdmissionFailures: stats.AdmissionFailures,
		Retried: stats.Retried, PrunedAge: stats.PrunedAge, PrunedBytes: stats.PrunedBytes,
		Malformed: stats.Malformed, Queue: loss,
	})
}

func writeReplayHealth(root string, event replayHealthEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	data = append(data, '\n')
	// stderr remains useful if the spool filesystem is full or inaccessible.
	_, _ = os.Stderr.Write(data)
	if err = appendReplayHealth(root, event.Stream, data); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, `{"event":"telemetry.export.health","status":"warning","causes":["health_file_unavailable"]}`)
	}
}

func appendReplayHealth(root, stream string, data []byte) error {
	if stream != "journal" && stream != "diagnostics" && stream != "traces" && stream != "export" {
		return fmt.Errorf("invalid health stream")
	}
	path := filepath.Join(root, "health-"+stream+".jsonl")
	lock, err := platformlock.TryAcquire(path + ".lock")
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	if err = lock.File().Chmod(0o600); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("invalid health file")
		}
		if info.Size()+int64(len(data)) > replayHealthFileLimit {
			if err = os.Remove(path + ".1"); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err = os.Rename(path, path+".1"); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	_, err = f.Write(data)
	return err // best effort; no fsync on the application path
}

func setReplayLossSource(target *atomic.Pointer[replayLossSource], sample func() replayLossCounters) {
	target.Store(&replayLossSource{sample: sample})
}
