package dispatcher

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/runnersolve"
)

// OS spellings, mirroring the runnersolve enum (product spellings, not GOOS).
const (
	osLinux   = runnersolve.OSLinux
	osWindows = runnersolve.OSWindows
)

// SelectionError is a dispatch-time refusal to place an attempt: the eligible
// set was empty, or the Windows structural facts (architecture §6/§11.7)
// emptied it. The diagnostic names the cause — it is journaled, so the
// refusal is diagnosable, never a silent non-placement.
type SelectionError struct {
	// Diagnostic is the named, human-readable reason.
	Diagnostic string
}

func (e *SelectionError) Error() string { return e.Diagnostic }

// SelectRunner picks the attempt's runner within the solver's eligible set
// (dispatcher §2 item 2): eligibility is the solver's (#3506); the dispatcher
// only picks WITHIN the set, Linux-preferring per the placement policy
// (dsl-3.0.md D2: placement prefers a Linux-class runner when the inventory
// has one), in inventory order.
//
// The Windows structural fact is re-asserted here rather than trusted to
// have happened upstream: a ledger-touching stage NEVER places on Windows
// (no path to the RWO instance root — architecture §6), so Windows runners
// are excluded for such a stage, and if that exclusion empties an otherwise
// non-empty set the refusal NAMES the fact (journaled/diagnosable,
// architecture §11.7).
func SelectRunner(attempt Attempt, eligible []RunnerSpec) (RunnerSpec, error) {
	if len(eligible) == 0 {
		return RunnerSpec{}, &SelectionError{Diagnostic: fmt.Sprintf(
			"stage %q (run %s, attempt %d): the solver's eligible runner set is empty — dispatch consumes eligibility, it never re-derives it",
			attempt.Stage, attempt.RunID, attempt.Number)}
	}

	candidates := eligible
	if attempt.LedgerTouching {
		filtered := make([]RunnerSpec, 0, len(eligible))
		var excluded []string
		for _, r := range eligible {
			if r.OS == osWindows {
				excluded = append(excluded, r.Name)
				continue
			}
			filtered = append(filtered, r)
		}
		if len(filtered) == 0 {
			return RunnerSpec{}, &SelectionError{Diagnostic: fmt.Sprintf(
				"stage %q (run %s, attempt %d) touches the instance ledger and can NEVER place on a Windows runner "+
					"(ledger-touching stages have no path to the RWO instance root — goobernetes-architecture.md §6); "+
					"every eligible runner is Windows: [%s]",
				attempt.Stage, attempt.RunID, attempt.Number, strings.Join(excluded, ", "))}
		}
		candidates = filtered
	}

	for _, r := range candidates {
		if r.OS == osLinux {
			return r, nil
		}
	}
	return candidates[0], nil
}

// CapacityTimeoutError is the bounded schedule-to-start failure (decision
// record D4 checkpoint 2): the runner was satisfiable but capacity never
// arrived within the bound. On Windows the diagnostic NAMES the costs being
// paid — scale-from-zero node provisioning and multi-GB image pulls — instead
// of reading as a generic timeout (architecture §6/§11.7, D12).
type CapacityTimeoutError struct {
	// Runner is the resolved runner that never gained capacity.
	Runner string
	// OS is the runner's OS (which bound applied).
	OS string
	// Waited is how long the dispatcher waited.
	Waited time.Duration
	// Bound is the schedule-to-start bound that expired.
	Bound time.Duration
	// LinuxDefault is the Linux default bound, cited when a Windows wait ran
	// past it.
	LinuxDefault time.Duration
}

func (e *CapacityTimeoutError) Error() string {
	if e.OS == osWindows {
		over := ""
		if e.Waited > e.LinuxDefault {
			over = fmt.Sprintf(" — past the Linux default bound of %s by design", e.LinuxDefault)
		}
		return fmt.Sprintf(
			"dispatcher: no capacity for Windows runner %q within the %s Windows schedule-to-start bound (waited %s%s): "+
				"Windows dispatch absorbs scale-from-zero node provisioning and multi-GB image pulls, which is why its bound is higher (D12); "+
				"if this recurs with a warm node pool, the pool is exhausted or its scaler is not adding Windows nodes",
			e.Runner, e.Bound, e.Waited.Round(time.Second), over)
	}
	return fmt.Sprintf(
		"dispatcher: no capacity for runner %q within the %s schedule-to-start bound (waited %s): "+
			"the runner was satisfiable but its pool is exhausted or scaled to zero (bounded, named runtime failure — decision record D4 checkpoint 2)",
		e.Runner, e.Bound, e.Waited.Round(time.Second))
}

// waitForCapacity blocks until the prober reports capacity or the OS-keyed
// schedule-to-start bound expires. A nil prober waits for nothing — the pod
// is created and Kubernetes queues it under its own activeDeadlineSeconds.
func (d *Dispatcher) waitForCapacity(ctx context.Context, runner RunnerSpec) error {
	if d.capacity == nil {
		return nil
	}
	bound := d.cfg.scheduleToStart(runner.OS)
	start := d.now()
	for {
		ok, err := d.capacity.Capacity(ctx, runner)
		if err != nil {
			return fmt.Errorf("dispatcher: probe capacity for runner %q: %w", runner.Name, err)
		}
		if ok {
			return nil
		}
		waited := d.now().Sub(start)
		if waited >= bound {
			return &CapacityTimeoutError{
				Runner:       runner.Name,
				OS:           runner.OS,
				Waited:       waited,
				Bound:        bound,
				LinuxDefault: d.cfg.linuxScheduleToStart(),
			}
		}
		if err := d.sleep(ctx, d.cfg.capacityInterval()); err != nil {
			return fmt.Errorf("dispatcher: capacity wait for runner %q interrupted: %w", runner.Name, err)
		}
	}
}
