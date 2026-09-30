package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

type startupHTTPProbe struct {
	Path                           string
	StartedAfterReadyMS, LatencyMS float64
	Status                         int
	Error                          string `json:",omitempty"`
}

type startupResponsiveness struct {
	StartedUTC                          time.Time
	RequestedWindowMS, ObservedWindowMS float64
	FirstWorkflowStartedAfterReadyMS    float64
	FirstWorkflowCommandMS              float64
	FirstWorkflowError                  string `json:",omitempty"`
	SourceRunEvents                     int
	SourceRuns, SourceCompletedRuns     int
	SourceJournalError                  string `json:",omitempty"`
	Probes                              []startupHTTPProbe
	// The startup receiver discards synthetic seeds. This mode measures API
	// and first-work responsiveness, not delivery completeness or CPU overhead.
	DeliveryReconciled bool
}

func (r *startupResponsiveness) Successful() bool {
	if r.FirstWorkflowError != "" || r.FirstWorkflowCommandMS <= 0 || r.SourceJournalError != "" || r.SourceRunEvents == 0 || r.SourceRuns != 1 || r.SourceCompletedRuns != 1 || len(r.Probes) < 2 {
		return false
	}
	for _, probe := range r.Probes {
		if probe.Error != "" || probe.Status != http.StatusOK {
			return false
		}
	}
	return true // Timing acceptance requires matched distributions, not this check.
}

func measureStartupResponsiveness(name, root, api string, duration time.Duration) *startupResponsiveness {
	started := time.Now()
	result := &startupResponsiveness{StartedUTC: started.UTC(), RequestedWindowMS: float64(duration) / float64(time.Millisecond)}
	workflowDone := make(chan struct{})
	go func() {
		defer close(workflowDone)
		workStart := time.Now()
		result.FirstWorkflowStartedAfterReadyMS = float64(workStart.Sub(started)) / float64(time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		output, err := cmd(ctx, "run", "--force", "--gaggle", "demo", "load0", root).CombinedOutput()
		result.FirstWorkflowCommandMS = float64(time.Since(workStart)) / float64(time.Millisecond)
		write(filepath.Join(out, name+"-post-ready-workflow.log"), string(output))
		if err != nil {
			result.FirstWorkflowError = err.Error()
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for time.Since(started) < duration {
		for _, path := range []string{"/readyz", "/api/v1/instance"} {
			result.Probes = append(result.Probes, startupResponseProbe(context.Background(), client, api, path, started))
		}
		if time.Since(started) < duration {
			<-ticker.C
		}
	}
	result.ObservedWindowMS = float64(time.Since(started)) / float64(time.Millisecond)
	<-workflowDone // The command has its own bound; never leave it orphaned.
	result.SourceRunEvents, result.SourceRuns, result.SourceCompletedRuns, result.SourceJournalError = startupWorkflowJournals(root)
	return result
}

func startupWorkflowJournals(root string) (events, runs, completed int, failure string) {
	paths, err := filepath.Glob(filepath.Join(root, "gaggles", "*", "runs", "*", "events.jsonl"))
	if err != nil {
		return 0, 0, 0, err.Error()
	}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return events, runs, completed, err.Error()
		}
		scan := bufio.NewScanner(f)
		scan.Buffer(make([]byte, 4096), 8<<20)
		var seq uint64
		terminals := 0
		for scan.Scan() {
			var event journal.Event
			if err = json.Unmarshal(scan.Bytes(), &event); err != nil || event.Seq != seq+1 {
				_ = f.Close()
				return events, runs, completed, fmt.Sprintf("invalid or nonconsecutive journal event in %s", path)
			}
			seq, events = event.Seq, events+1
			if event.Type == journal.EventRunFinished {
				terminals++
				if event.Status != string(journal.PhaseCompleted) {
					_ = f.Close()
					return events, runs, completed, "workflow journal reports a non-completed terminal outcome"
				}
			}
		}
		scanErr, closeErr := scan.Err(), f.Close()
		if scanErr != nil {
			return events, runs, completed, scanErr.Error()
		}
		if closeErr != nil {
			return events, runs, completed, closeErr.Error()
		}
		runs++
		if terminals != 1 {
			return events, runs, completed, "workflow journal needs exactly one completed terminal event"
		}
		completed++
	}
	return events, runs, completed, ""
}

func startupResponseProbe(ctx context.Context, httpClient *http.Client, api, path string, ready time.Time) startupHTTPProbe {
	started := time.Now()
	result := startupHTTPProbe{Path: path, StartedAfterReadyMS: float64(started.Sub(ready)) / float64(time.Millisecond)}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, api+path, nil)
	if err == nil {
		var response *http.Response
		response, err = httpClient.Do(request)
		if response != nil {
			result.Status = response.StatusCode
			_, bodyErr := io.Copy(io.Discard, response.Body)
			closeErr := response.Body.Close()
			if err == nil {
				err = bodyErr
				if err == nil {
					err = closeErr
				}
			}
		}
	}
	result.LatencyMS = float64(time.Since(started)) / float64(time.Millisecond)
	if err != nil {
		result.Error = err.Error()
	}
	return result
}
