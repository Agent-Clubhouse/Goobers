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

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/platform/durability"
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
// unique temp name os.CreateTemp mints, so concurrent `goobers run`
// invocations against the same instance never collide without needing any
// extra locking of their own). The request is published atomically — written
// to a hidden temp that does NOT match requestSuffix, then renamed into place —
// so the daemon's sweep can never observe (and reject as malformed) a
// half-written request, the same torn-read guard claims.go and runcancel.go
// use. Before this was atomic, os.CreateTemp minted the request file already
// named *.request.json, so a sweep landing between create and write read empty
// bytes and failed the delegation.
func writeTriggerRequest(schedulerDir, workflow string) (requestID string, err error) {
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		return "", fmt.Errorf("delegate: create pending-triggers dir: %w", err)
	}
	f, err := os.CreateTemp(reqDir, ".pending-*")
	if err != nil {
		return "", fmt.Errorf("delegate: create trigger request: %w", err)
	}
	tmpPath := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}

	data, err := json.Marshal(triggerRequest{Workflow: workflow, CreatedAt: time.Now().UTC()})
	if err != nil {
		cleanup()
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return "", fmt.Errorf("delegate: write trigger request: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("delegate: close trigger request: %w", err)
	}
	requestID = strings.TrimPrefix(filepath.Base(tmpPath), ".pending-")
	finalPath := filepath.Join(reqDir, requestID+requestSuffix)
	if err := durability.ReplaceFile(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("delegate: publish trigger request: %w", err)
	}
	return requestID, nil
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
			// The writer (sweepPendingTriggers / a test responder) uses a plain,
			// non-atomic os.WriteFile, so this read can land in the window between
			// the O_TRUNC that empties the file and the content being fully
			// written — yielding empty or partial bytes that don't parse. Treat
			// that as "not ready yet" and re-poll rather than failing the whole
			// delegation: a torn read is transient, and consuming (removing) the
			// file before a clean parse would strand the real response so the next
			// poll could never see it. Only remove once we have a complete,
			// parseable response. The deadline still bounds a genuinely stuck writer.
			var resp triggerResponse
			if jerr := json.Unmarshal(data, &resp); jerr == nil {
				_ = os.Remove(respPath)
				if resp.Error != "" {
					return "", errors.New(resp.Error)
				}
				return resp.RunID, nil
			}
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
// protocol, called at startup and periodically from runUpContext's sweep
// goroutine
// (mirroring the existing claim-recovery ticker's shape). It dispatches
// every pending request through sched — the exact same Scheduler.Trigger a
// local `goobers run` invocation would call directly — and writes back a
// response for pollTriggerResponse to consume.
//
// A request file is removed BEFORE dispatch, not after: if the daemon
// crashed mid-dispatch, a still-present request file would replay on the
// next process's startup sweep and double-trigger the same nominal request;
// removing first means a lost response in that narrow window fails the
// waiting `goobers run` closed (timeout) rather than risking a duplicate
// run — the same "don't replay an ambiguous firing" principle Scheduler's
// own trigger.fired-before-dispatch ordering already applies (see dispatch's
// doc comment in scheduler.go).
func sweepPendingTriggers(ctx context.Context, schedulerDir string, sched *localscheduler.Scheduler, now func() time.Time) error {
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	entries, exists, err := readDirectory(reqDir)
	if !exists {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delegate: read pending triggers: %w", err)
	}
	var sweepErr error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), responseSuffix) {
			info, err := e.Info()
			if err == nil && now().Sub(info.ModTime()) > triggerDelegationTimeout {
				_ = os.Remove(filepath.Join(reqDir, e.Name()))
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), requestSuffix) {
			continue
		}

		requestID := strings.TrimSuffix(e.Name(), requestSuffix)
		reqPath := filepath.Join(reqDir, e.Name())

		data, err := os.ReadFile(reqPath)
		if err != nil {
			if !os.IsNotExist(err) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("delegate: read trigger request %s: %w", requestID, err))
			}
			continue
		}
		if err := os.Remove(reqPath); err != nil {
			if !os.IsNotExist(err) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("delegate: consume trigger request %s: %w", requestID, err))
			}
			continue
		}

		var req triggerRequest
		resp := triggerResponse{}
		if err := json.Unmarshal(data, &req); err != nil {
			resp.Error = fmt.Sprintf("delegate: malformed trigger request: %v", err)
		} else if req.CreatedAt.IsZero() {
			resp.Error = fmt.Sprintf("delegate: trigger request %s has no creation time; refusing to dispatch", requestID)
			sched.RecordTriggerRefusal(req.Workflow, resp.Error)
		} else {
			sweepTime := now()
			if sweepTime.Sub(req.CreatedAt) > triggerDelegationTimeout {
				resp.Error = fmt.Sprintf(
					"delegate: stale trigger request %s was created at %s, more than %s ago; refusing to dispatch",
					requestID, req.CreatedAt.Format(time.RFC3339Nano), triggerDelegationTimeout,
				)
				sched.RecordTriggerRefusal(req.Workflow, resp.Error)
			} else {
				runID, terr := sched.Trigger(ctx, req.Workflow, sweepTime)
				var rejected *localscheduler.TriggerRejectedError
				switch {
				case terr != nil && errors.As(terr, &rejected) && rejected.Transient():
					// A capacity refusal is held by a run that is already
					// finishing, so answering the client with a hard error
					// turns a moment of contention into a failed command. Put
					// the request back, untouched, and let the next sweep try
					// again: CreatedAt is preserved, so the staleness check
					// above still bounds the wait, and the client's own
					// pollTriggerResponse deadline bounds it independently.
					if rerr := os.WriteFile(reqPath, data, 0o644); rerr != nil {
						sweepErr = errors.Join(sweepErr, fmt.Errorf("delegate: requeue trigger request %s: %w", requestID, rerr))
						resp.Error = terr.Error()
						break
					}
					continue
				case terr != nil:
					resp.Error = terr.Error()
				default:
					resp.RunID = runID
				}
			}
		}

		respData, err := json.Marshal(resp)
		if err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("delegate: encode trigger response %s: %w", requestID, err))
			continue
		}
		if err := os.WriteFile(filepath.Join(reqDir, requestID+responseSuffix), respData, 0o644); err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("delegate: write trigger response %s: %w", requestID, err))
		}
	}
	return sweepErr
}
