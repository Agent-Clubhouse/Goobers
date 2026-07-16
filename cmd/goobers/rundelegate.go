package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// rundelegate.go implements #343: when a short-lived `goobers run` process
// finds a live `goobers up` daemon already holding this instance's up.lock,
// it no longer just fails — it hands the trigger off to that daemon through
// a small file-based request/response protocol under
// <SchedulerDir>/pending-triggers/, and the daemon's own periodic sweep
// (wired in up.go) dispatches it through the exact same Scheduler.Trigger
// path a local `goobers run` would have used itself.
//
// This is deliberately NOT built on #169's planned daemon HTTP API — #169 is
// unbuilt and explicitly gated ("do not dispatch until its design review"),
// so depending on it would either mean taking on unreviewed V1 design work
// or inventing a parallel ad-hoc HTTP surface that risks conflicting with
// #169's eventual shape. Reusing the daemon's own already-safe-for-
// concurrent-calls Scheduler (Trigger/Tick already interleave safely under
// its internal mutex — see scheduler.go's Tick doc comment) and a periodic
// filesystem sweep (the same idle-between-ticks philosophy the scheduler
// loop itself uses, no busy-polling) needs no new server, port, or auth
// surface at all.

// pendingTriggersDir is the SchedulerDir subdirectory delegation request/
// response files live under.
const pendingTriggersDir = "pending-triggers"

// triggerRequest is one delegation request file's content: "please Trigger
// this workflow on my behalf, I couldn't take the lock myself."
type triggerRequest struct {
	Workflow  string    `json:"workflow"`
	CreatedAt time.Time `json:"createdAt"`
}

// triggerResponse is what the daemon writes back once it has acted on a
// triggerRequest — exactly one of RunID/Error is set (mirroring
// Scheduler.Trigger's own (runID, err) return shape).
type triggerResponse struct {
	RunID string `json:"runId,omitempty"`
	Error string `json:"error,omitempty"`
}

// requestSuffix/responseSuffix name a request/response file pair sharing one
// request id: "<id>.request.json" / "<id>.response.json".
const (
	requestSuffix  = ".request.json"
	responseSuffix = ".response.json"
)

// writeTriggerRequest drops a new delegation request file under
// schedulerDir/pending-triggers and returns its request id (derived from the
// unique temp file os.CreateTemp mints, so concurrent `goobers run`
// invocations against the same instance never collide without needing any
// extra locking of their own).
func writeTriggerRequest(schedulerDir, workflow string) (requestID string, err error) {
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		return "", fmt.Errorf("delegate: create pending-triggers dir: %w", err)
	}
	f, err := os.CreateTemp(reqDir, "*"+requestSuffix)
	if err != nil {
		return "", fmt.Errorf("delegate: create trigger request: %w", err)
	}
	defer func() { _ = f.Close() }()

	data, err := json.Marshal(triggerRequest{Workflow: workflow, CreatedAt: time.Now().UTC()})
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		return "", fmt.Errorf("delegate: write trigger request: %w", err)
	}
	return strings.TrimSuffix(filepath.Base(f.Name()), requestSuffix), nil
}

// pollTriggerResponse waits for schedulerDir/pending-triggers/<requestID>
// .response.json to appear (the daemon's sweep writes it once it has
// dispatched — or failed to dispatch — the request), consumes it, and
// returns the same (runID, err) shape Scheduler.Trigger itself returns. A
// timeout — not an indefinite wait — bounds the case where no live daemon is
// actually picking requests up (e.g. it exited between this process
// observing up.lock held and writing its request).
func pollTriggerResponse(ctx context.Context, schedulerDir, requestID string, timeout time.Duration) (runID string, err error) {
	respPath := filepath.Join(schedulerDir, pendingTriggersDir, requestID+responseSuffix)
	deadline := time.Now().Add(timeout)
	for {
		if data, rerr := os.ReadFile(respPath); rerr == nil {
			_ = os.Remove(respPath)
			var resp triggerResponse
			if jerr := json.Unmarshal(data, &resp); jerr != nil {
				return "", fmt.Errorf("delegate: malformed response for request %s: %w", requestID, jerr)
			}
			if resp.Error != "" {
				return "", errors.New(resp.Error)
			}
			return resp.RunID, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("delegate: timed out after %s waiting for the live `goobers up` daemon to pick up the trigger request "+
				"(request left at %s — is the daemon still running and healthy?)", timeout, filepath.Join(schedulerDir, pendingTriggersDir, requestID+requestSuffix))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(delegationPollInterval):
		}
	}
}

// delegationPollInterval bounds how often pollTriggerResponse re-checks for
// a response file. Var, not const, so tests aren't slow.
var delegationPollInterval = 100 * time.Millisecond

// triggerDelegationTimeout bounds pollTriggerResponse's total wait. Var, not
// const, for the same reason. 30s comfortably exceeds delegationSweepInterval
// (up.go) by a wide margin under any normal daemon load.
var triggerDelegationTimeout = 30 * time.Second

// sweepPendingTriggers is the daemon-side half of #343's delegation
// protocol, called periodically from runUpContext's own sweep goroutine
// (mirroring the existing claim-recovery ticker's shape). It dispatches
// every fresh pending request through sched — the exact same Scheduler.Trigger
// a local `goobers run` invocation would call directly — and writes back a
// response for pollTriggerResponse to consume. Requests that have outlived the
// client's wait are refused rather than replayed, and orphaned responses are
// removed after the same bound.
//
// A request file is removed BEFORE dispatch, not after: if the daemon
// crashed mid-dispatch, a still-present request file would replay on the
// next process's startup sweep and double-trigger the same nominal request;
// removing first means a lost response in that narrow window fails the
// waiting `goobers run` closed (timeout) rather than risking a duplicate
// run — the same "don't replay an ambiguous firing" principle Scheduler's
// own trigger.fired-before-dispatch ordering already applies (see dispatch's
// doc comment in scheduler.go).
func sweepPendingTriggers(ctx context.Context, schedulerDir string, sched *localscheduler.Scheduler, log *journal.InstanceLog, now func() time.Time) {
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	entries, err := os.ReadDir(reqDir)
	if err != nil {
		return // no pending-triggers dir yet (no delegated request has ever been made) — nothing to do
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		reqPath := filepath.Join(reqDir, e.Name())
		if strings.HasSuffix(e.Name(), responseSuffix) {
			info, err := e.Info()
			if err == nil && now().After(info.ModTime().Add(triggerDelegationTimeout)) {
				_ = os.Remove(reqPath)
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), requestSuffix) {
			continue
		}
		requestID := strings.TrimSuffix(e.Name(), requestSuffix)

		data, err := os.ReadFile(reqPath)
		if err != nil {
			continue // gone already (a concurrent sweep somehow won it, or it was cleaned up) — skip, nothing to respond to
		}
		if err := os.Remove(reqPath); err != nil {
			continue // lost a race for this exact file; the winner will respond
		}

		var req triggerRequest
		resp := triggerResponse{}
		sweptAt := now()
		if err := json.Unmarshal(data, &req); err != nil {
			resp.Error = fmt.Sprintf("delegate: malformed trigger request: %v", err)
		} else if req.CreatedAt.IsZero() {
			const reason = "delegation: trigger request missing createdAt"
			resp.Error = "delegate: malformed trigger request: missing createdAt"
			if log != nil {
				_ = log.Append(journal.Event{
					Type:     journal.EventTickSkipped,
					Workflow: req.Workflow,
					Reason:   reason,
				})
			}
		} else if sweptAt.After(req.CreatedAt.Add(triggerDelegationTimeout)) {
			const reason = "delegation: trigger request expired before daemon pickup"
			resp.Error = "delegate: trigger request expired before live daemon pickup"
			if log != nil {
				_ = log.Append(journal.Event{
					Type:     journal.EventTickSkipped,
					Workflow: req.Workflow,
					Reason:   reason,
				})
			}
		} else {
			runID, terr := sched.Trigger(ctx, req.Workflow, sweptAt)
			if terr != nil {
				resp.Error = terr.Error()
			} else {
				resp.RunID = runID
			}
		}

		respData, err := json.Marshal(resp)
		if err != nil {
			continue // shouldn't happen (triggerResponse is trivially marshalable); the waiting `goobers run` times out instead
		}
		_ = os.WriteFile(filepath.Join(reqDir, requestID+responseSuffix), respData, 0o644)
	}
}
