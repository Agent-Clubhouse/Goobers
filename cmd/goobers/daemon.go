package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/workflow"
)

// schedulerSetup bundles everything both `up` and `run` need to build a
// localscheduler.Scheduler over an instance's config: the shared runner, the
// telemetry client both the runner and the scheduler span through, the
// telemetry rollup every dispatched run incrementally ingests into (issue
// #127), its instance log, and one WorkflowEntry per configured workflow.
// Factored out so both commands construct it identically (issue #134: `run`
// used to build its own bare *runner.Runner and skip the scheduler/
// conditions/journal/lock entirely — the two commands must agree on this
// construction, not maintain two divergent copies of it). The caller owns
// calling Telemetry.Shutdown and RollupDB.Close once it's done driving runs,
// exactly as it did before this seam existed.
type schedulerSetup struct {
	Runner      *runner.Runner
	Telemetry   *telemetry.Client
	RollupDB    *rollup.DB
	InstanceLog *journal.InstanceLog
	Entries     []localscheduler.WorkflowEntry
	Machines    map[string]*workflow.Machine
	RepoRefs    map[string]apiv1.RepoRef
}

// buildSchedulerSetup loads an instance's config, compiles its workflows,
// resolves their RepoRefs, constructs the shared runner, telemetry client,
// and telemetry rollup, and builds one localscheduler.WorkflowEntry per
// workflow — everything localscheduler.New needs. wg is threaded into every
// entry's trackedStarter so a caller (up's daemon loop, or run's single
// foreground trigger) can track dispatched runs uniformly.
func buildSchedulerSetup(ctx context.Context, l instance.Layout, wg *sync.WaitGroup) (*schedulerSetup, error) {
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return nil, err
	}
	set, _, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil {
		return nil, fmt.Errorf("config directory invalid: %w", err)
	}

	goobers := goobersByName(set)
	machines, err := compiledMachines(set, goobers)
	if err != nil {
		return nil, err
	}
	repoRefs, err := repoRefsByWorkflow(set)
	if err != nil {
		return nil, err
	}

	// telemetry.enabled defaults to true; instance.yaml can opt out (issue
	// #129). tel/rollupDB stay nil in that case — every downstream use
	// already tolerates nil: buildRunnerConfig only sets
	// runner.Config.Telemetry when tel != nil, ingestRunTelemetry no-ops on a
	// nil *rollup.DB, and SchedulerOptions/Shutdown below no-op too. A nil
	// *telemetry.Client must never reach localscheduler.WithTelemetry
	// directly — that would wrap it in a non-nil SpanStarter interface value
	// (Go's typed-nil-in-interface trap), making localscheduler's own
	// `s.telemetry == nil` guard wrongly evaluate false and panic on first
	// use; SchedulerOptions is the one place that decision is made.
	var tel *telemetry.Client
	var rollupDB *rollup.DB
	if cfg.TelemetryEnabled() {
		tel, err = buildTelemetryClient(ctx, l)
		if err != nil {
			return nil, err
		}
		rollupDB, err = rollup.Open(l.TelemetryDB())
		if err != nil {
			return nil, err
		}
	}

	runnerCfg, err := buildRunnerConfig(l, cfg, goobers, tel)
	if err != nil {
		return nil, err
	}
	rn, err := runner.New(runnerCfg)
	if err != nil {
		return nil, err
	}

	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		return nil, fmt.Errorf("open instance log: %w", err)
	}

	entries := make([]localscheduler.WorkflowEntry, 0, len(set.Workflows))
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		var sched localscheduler.Schedule
		for _, tr := range wf.Spec.Triggers {
			if tr.Type == apiv1.TriggerSchedule && tr.Schedule != "" {
				s, err := localscheduler.ParseSchedule(tr.Schedule)
				if err != nil {
					return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
				}
				sched = s
				break
			}
		}
		entries = append(entries, localscheduler.WorkflowEntry{
			Workflow:  wf.Name,
			Gaggle:    wf.Spec.Gaggle,
			Readiness: wf.Spec.Readiness,
			Schedule:  sched,
			Starter:   &trackedStarter{r: rn, machine: machines[wf.Name], wg: wg, l: l, tel: tel, rollupDB: rollupDB},
			RepoRef:   repoRefs[wf.Name],
		})
	}

	return &schedulerSetup{
		Runner:      rn,
		Telemetry:   tel,
		RollupDB:    rollupDB,
		InstanceLog: instanceLog,
		Entries:     entries,
		Machines:    machines,
		RepoRefs:    repoRefs,
	}, nil
}

// SchedulerOptions returns the localscheduler.Option slice reflecting this
// setup's telemetry state — empty when telemetry is disabled (issue #129).
// See buildSchedulerSetup's doc comment for why a nil Telemetry must never
// reach localscheduler.WithTelemetry directly.
func (s *schedulerSetup) SchedulerOptions() []localscheduler.Option {
	if s.Telemetry == nil {
		return nil
	}
	return []localscheduler.Option{localscheduler.WithTelemetry(s.Telemetry)}
}

// Shutdown flushes/closes the telemetry client and rollup db, nil-safe so a
// caller can defer it unconditionally regardless of whether instance.yaml
// enabled telemetry (issue #129).
func (s *schedulerSetup) Shutdown(ctx context.Context) {
	if s.Telemetry != nil {
		_ = s.Telemetry.Shutdown(ctx)
	}
	if s.RollupDB != nil {
		_ = s.RollupDB.Close()
	}
}

// trackedStarter adapts a *runner.Runner + its compiled Machine into a
// localscheduler.Starter — one per workflow, per that seam's doc comment
// ("#17's *runner.Runner is bound to a single compiled machine at
// construction, so the scheduler holds a map of workflow name -> Starter").
// It also tracks every dispatched run in wg so the daemon's shutdown drain
// (runUpContext) waits for scheduler-dispatched runs, not just the startup
// resume scan's — wg.Add happens inside Start, which localscheduler's own
// dispatch already calls from its own goroutine, so there is an inherent
// (and accepted) small race window between that goroutine launching and
// wg.Add actually running; closing it fully would need a scheduler-side
// hook this seam doesn't expose. Every dispatch through this Starter — both
// `goobers up`'s scheduled/manual-via-Trigger fires and `goobers run`'s own
// sched.Trigger call, now that #134 routes it through the same scheduler —
// incrementally ingests into rollupDB on completion (issue #127).
type trackedStarter struct {
	r        *runner.Runner
	machine  *workflow.Machine
	wg       *sync.WaitGroup
	l        instance.Layout
	tel      *telemetry.Client
	rollupDB *rollup.DB
}

func (s *trackedStarter) Start(ctx context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
	s.wg.Add(1)
	defer s.wg.Done()
	res, err := s.r.Start(ctx, runner.StartInput{
		RunID:   req.RunID,
		Machine: s.machine,
		Gaggle:  req.Gaggle,
		Trigger: req.Trigger,
		RepoRef: req.RepoRef,
		Item:    req.Item,
	})
	ingestRunTelemetry(s.tel, s.rollupDB, s.l, req.RunID)
	return localscheduler.StartResult{Phase: res.Phase, FinalState: res.FinalState}, err
}

// resumeInterruptedRuns scans runsDir for any run left non-terminal by a
// prior crash or unclean daemon shutdown and restarts it via Runner.Resume,
// each in its own goroutine tracked by wg — the daemon-startup recovery pass
// (issue #23 AC: restart via Runner.Resume). "Interrupted" is exactly
// journal.PhaseRunning (or an unreadable state.json, conservatively treated
// the same way ActiveRunCounts does): no run.finished event has landed.
// Resume itself is idempotent on an already-terminal run and safe to call on
// one that merely paused gracefully (a human gate, or a prior clean drain),
// not only a genuine crash — so this scan doesn't need to distinguish those
// cases itself; a gate-paused run's Resume call returns almost immediately
// (walk re-checkpoints at the same gate without evaluating anything), so its
// reserved slot (below) is held only briefly, not for the daemon's lifetime.
//
// release is called with each resumed run's workflow once its Resume call
// returns (success or error) — the counterpart to Scheduler.Reconcile having
// already seeded that run's workflow into Conditions' active count (issue
// #135: previously nothing ever released a reconciled slot, so any restart
// with a non-terminal run starved that workflow of new dispatches forever).
// Pass sched.Release; a plain func so this doesn't need a *Scheduler to test.
//
// A run whose workflow no longer resolves in the current config (renamed or
// removed, issue #135 point 2) is skipped with a warning journaled to log,
// not a fatal error — a stale run must never prevent the daemon from
// starting; recovering it is `goobers run abort <run-id>` (abort.go).
//
// Each resumed run also incrementally ingests into rollupDB once its outcome
// is known (issue #127), the same hook trackedStarter.Start uses for a live
// dispatch — a resumed run's spans/errors/stage_attempts must show up in
// `goobers telemetry` too, not just a freshly-dispatched one's. tel is
// flushed first (issue #129), same ordering rationale as
// trackedStarter.Start — the batched span exporter must write spans.jsonl to
// disk before ingest reads it.
//
// resumeInterruptedRuns itself only errors on something that makes the scan
// as a whole meaningless (runsDir unreadable for a reason other than not
// existing yet).
func resumeInterruptedRuns(ctx context.Context, l instance.Layout, rn *runner.Runner, machines map[string]*workflow.Machine, repoRefs map[string]apiv1.RepoRef, log *journal.InstanceLog, tel *telemetry.Client, rollupDB *rollup.DB, release func(workflow string), wg *sync.WaitGroup) (resumed []string, warned []string, err error) {
	runsDir := l.RunsDir()
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read runs directory: %w", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(runsDir, e.Name())
		rd, err := journal.OpenRead(dir)
		if err != nil {
			continue // not a run directory
		}
		id, err := rd.Identity()
		if err != nil {
			continue
		}
		if st, err := rd.State(); err == nil {
			switch st.Phase {
			case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
				continue // terminal: nothing to resume
			}
		}

		machine, ok := machines[id.Workflow]
		if !ok {
			warned = append(warned, id.RunID)
			if log != nil {
				_ = log.Append(journal.Event{
					Type: journal.EventError, Workflow: id.Workflow, RunID: id.RunID,
					Error: &journal.ErrorDetail{
						Code:    "resume_unresolvable_workflow",
						Message: fmt.Sprintf("run %q references unknown workflow %q — recover with `goobers run abort %s`", id.RunID, id.Workflow, id.RunID),
					},
				})
			}
			continue
		}
		repoRef := repoRefs[id.Workflow]

		resumed = append(resumed, id.RunID)
		wg.Add(1)
		go func(runID, wfName string) {
			defer wg.Done()
			defer release(wfName)
			result, err := rn.Resume(ctx, runner.ResumeInput{RunID: runID, Machine: machine, RepoRef: repoRef})
			ingestRunTelemetry(tel, rollupDB, l, runID)
			status := string(result.Phase)
			if err != nil {
				status = "error: " + err.Error()
			}
			if log != nil {
				_ = log.Append(journal.Event{Type: journal.EventRunFinished, Workflow: wfName, RunID: runID, Status: status})
			}
		}(id.RunID, id.Workflow)
	}
	return resumed, warned, nil
}

// waitDrained waits for wg to finish, returning false if timeout elapses
// first. The background goroutine it starts is not leaked: wg.Wait()
// returning always lets it close done and exit, whether or not the select
// below already gave up waiting.
func waitDrained(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
