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
	"github.com/goobers/goobers/internal/worktree"
)

// schedulerSetup bundles everything both `up` and `run` need to build a
// localscheduler.Scheduler over an instance's config: the shared runner, the
// telemetry client both the runner and the scheduler span through, the
// telemetry rollup every dispatched run incrementally ingests into (issue
// #127), the worktree.Manager the runner dispatches through, its instance
// log, and one WorkflowEntry per configured workflow. Factored out so both
// commands construct it identically (issue #134: `run` used to build its own
// bare *runner.Runner and skip the scheduler/conditions/journal/lock
// entirely — the two commands must agree on this construction, not maintain
// two divergent copies of it). The caller owns calling Telemetry.Shutdown and
// RollupDB.Close once it's done driving runs, exactly as it did before this
// seam existed.
type schedulerSetup struct {
	Runner        *runner.Runner
	Telemetry     *telemetry.Client
	RollupDB      *rollup.DB
	Worktrees     *worktree.Manager
	InstanceLog   *journal.InstanceLog
	Entries       []localscheduler.WorkflowEntry
	Machines      map[string]*workflow.Machine
	RepoRefs      map[string]apiv1.RepoRef
	RunConditions instance.RunConditions
	// OpenPRRefresher backs the #353 MaxOpenPRs cap; nil when no workflow opts
	// in (or no repo is configured). Only the `up` daemon starts its Run loop
	// and wires it as a scheduler option — see up.go.
	OpenPRRefresher *localscheduler.OpenPRRefresher
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
	// Fail fast if an agentic stage's harness isn't usable (missing/broken/
	// signed-out CLI) — before any worktree/claim/journal side effect — rather
	// than burning a mid-run agentic attempt (#238). Indirected through the
	// preflightHarnesses seam so the cmd/goobers test suite (which drives up/run
	// without a real, installed Copilot CLI) can neutralize it uniformly; the
	// real preflight logic is covered directly by preflight_test.go.
	if err := preflightHarnesses(goobers, set.Workflows); err != nil {
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
	// One instance-global registry, fed by every run's resolved credentials (via
	// the teeRegistrar in buildRunnerConfig) and chained before the pattern net.
	// It is what lets the span exporter and instance log — both instance-lifetime,
	// outliving any single run — redact resolver-issued secrets by exact value,
	// not just by shape (#117 Piece B). Registry redaction is concurrent-safe and
	// keyed by digest, so many runs feeding it is fine.
	sharedReg := journal.NewRegistryScrubber()
	sharedScrubber := journal.Chain(sharedReg, journal.NewPatternScrubber())

	var tel *telemetry.Client
	var rollupDB *rollup.DB
	if cfg.TelemetryEnabled() {
		tel, err = buildTelemetryClient(ctx, l, sharedScrubber)
		if err != nil {
			return nil, err
		}
		rollupDB, err = rollup.Open(l.TelemetryDB())
		if err != nil {
			return nil, err
		}
	}

	runnerCfg, wtMgr, err := buildRunnerConfig(l, cfg, goobers, tel, sharedReg)
	if err != nil {
		return nil, err
	}
	rn, err := runner.New(runnerCfg)
	if err != nil {
		return nil, err
	}

	// #353: the open-PR-count refresher backing the MaxOpenPRs cap, when any
	// workflow opts in. Built here (has cfg + the compiled workflows); the `up`
	// daemon starts its Run loop and wires it as a scheduler option.
	openPRRefresher, err := buildOpenPRRefresher(cfg, set.Workflows, sharedReg)
	if err != nil {
		return nil, err
	}

	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir(), journal.WithScrubber(sharedScrubber))
	if err != nil {
		return nil, fmt.Errorf("open instance log: %w", err)
	}

	// Issue #137: every workflow's cron schedule evaluates in the
	// instance-configured timezone (Config.Timezone, default UTC), not
	// whatever the host process's own local zone happens to be — InLocation
	// already does the right thing with a restart-reconstructed LastEval
	// too, since it normalizes via time.Time.In(loc) before any wall-clock
	// field matching, regardless of what zone that reconstructed time was
	// itself expressed in (a JSON-round-tripped fixed UTC offset).
	loc, err := cfg.Location()
	if err != nil {
		return nil, err
	}

	entries := make([]localscheduler.WorkflowEntry, 0, len(set.Workflows))
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		// #341: a workflow may declare more than one schedule-type trigger
		// (e.g. a weekday cadence and a separate weekend one) — collect all
		// of them rather than stopping at the first; Scheduler.Tick fires if
		// any is due. #342: also collect every signal-type trigger's name —
		// previously compiled nowhere, so a type=signal trigger declared in
		// config did nothing at runtime; Scheduler.Signal fires every
		// workflow subscribed to a received signal name.
		var scheds []localscheduler.Schedule
		var sigs []string
		for _, tr := range wf.Spec.Triggers {
			if tr.Type == apiv1.TriggerSchedule && tr.Schedule != "" {
				s, err := localscheduler.ParseSchedule(tr.Schedule)
				if err != nil {
					return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
				}
				scheds = append(scheds, localscheduler.InLocation(s, loc))
			}
			if tr.Type == apiv1.TriggerSignal && tr.Signal != "" {
				sigs = append(sigs, tr.Signal)
			}
		}
		entries = append(entries, localscheduler.WorkflowEntry{
			Workflow:  wf.Name,
			Gaggle:    wf.Spec.Gaggle,
			Readiness: wf.Spec.Readiness,
			Schedules: scheds,
			Signals:   sigs,
			Starter:   &trackedStarter{r: rn, machine: machines[wf.Name], wg: wg, l: l, tel: tel, rollupDB: rollupDB, log: instanceLog},
			RepoRef:   repoRefs[wf.Name],
		})
	}

	return &schedulerSetup{
		Runner:          rn,
		Telemetry:       tel,
		RollupDB:        rollupDB,
		Worktrees:       wtMgr,
		InstanceLog:     instanceLog,
		Entries:         entries,
		Machines:        machines,
		RepoRefs:        repoRefs,
		RunConditions:   cfg.RunConditions,
		OpenPRRefresher: openPRRefresher,
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
	log      *journal.InstanceLog
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
	ingestRunTelemetry(s.tel, s.rollupDB, s.l, req.RunID, s.log)
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
		// Event-log-first (#242): state.json can lag a crash-fsynced
		// run.finished event, so Phase() (reconstructed from the log) is
		// what decides whether this run is actually terminal — trusting
		// the checkpoint directly here risks spinning up a resume
		// goroutine for a run that already finished.
		if phase, err := rd.Phase(); err == nil {
			switch phase {
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
			ingestRunTelemetry(tel, rollupDB, l, runID, log)
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
