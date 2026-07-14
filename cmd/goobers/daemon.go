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
	"github.com/goobers/goobers/internal/workflow"
)

// schedulerSetup bundles everything both `up` and `run` need to build a
// localscheduler.Scheduler over an instance's config: the shared runner, the
// telemetry client both the runner and the scheduler span through, its
// instance log, and one WorkflowEntry per configured workflow. Factored out
// so both commands construct it identically (issue #134: `run` used to
// build its own bare *runner.Runner and skip the scheduler/conditions/
// journal/lock entirely — the two commands must agree on this construction,
// not maintain two divergent copies of it). The caller owns calling
// Telemetry.Shutdown once it's done driving runs, exactly as it did before
// this seam existed.
type schedulerSetup struct {
	Runner      *runner.Runner
	Telemetry   *telemetry.Client
	InstanceLog *journal.InstanceLog
	Entries     []localscheduler.WorkflowEntry
	Machines    map[string]*workflow.Machine
	RepoRefs    map[string]apiv1.RepoRef
}

// buildSchedulerSetup loads an instance's config, compiles its workflows,
// resolves their RepoRefs, constructs the shared runner and telemetry
// client, and builds one localscheduler.WorkflowEntry per workflow —
// everything localscheduler.New needs. wg is threaded into every entry's
// trackedStarter so a caller (up's daemon loop, or run's single foreground
// trigger) can track dispatched runs uniformly.
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

	tel, err := buildTelemetryClient(ctx, l)
	if err != nil {
		return nil, err
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
			Starter:   &trackedStarter{r: rn, machine: machines[wf.Name], wg: wg},
			RepoRef:   repoRefs[wf.Name],
		})
	}

	return &schedulerSetup{
		Runner:      rn,
		Telemetry:   tel,
		InstanceLog: instanceLog,
		Entries:     entries,
		Machines:    machines,
		RepoRefs:    repoRefs,
	}, nil
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
// hook this seam doesn't expose.
type trackedStarter struct {
	r       *runner.Runner
	machine *workflow.Machine
	wg      *sync.WaitGroup
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
// cases itself.
//
// A resume failure has nowhere synchronous to report to (this runs
// concurrently with the caller going on to start the scheduler) — it is
// journaled to the instance log with the same run.finished/error convention
// localscheduler's own dispatch uses for a failed Start, so it is visible
// via the instance journal rather than silently dropped.
func resumeInterruptedRuns(ctx context.Context, runsDir string, rn *runner.Runner, machines map[string]*workflow.Machine, repoRefs map[string]apiv1.RepoRef, log *journal.InstanceLog, wg *sync.WaitGroup) ([]string, error) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read runs directory: %w", err)
	}

	var resumed []string
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
			return nil, fmt.Errorf("interrupted run %q references unknown workflow %q", id.RunID, id.Workflow)
		}
		repoRef := repoRefs[id.Workflow]

		resumed = append(resumed, id.RunID)
		wg.Add(1)
		go func(runID, wfName string) {
			defer wg.Done()
			_, err := rn.Resume(ctx, runner.ResumeInput{RunID: runID, Machine: machine, RepoRef: repoRef})
			status := "resumed"
			if err != nil {
				status = "error: " + err.Error()
			}
			if log != nil {
				_ = log.Append(journal.Event{Type: journal.EventRunFinished, Workflow: wfName, RunID: runID, Status: status})
			}
		}(id.RunID, id.Workflow)
	}
	return resumed, nil
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
