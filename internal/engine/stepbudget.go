package engine

import "fmt"

// walkStepBudget is workflow state shared by the root and all branch bodies.
// Charge never yields: concurrent workflow coroutines cannot race this counter.
// Physical retries stay within the stage's existing attempt budget.
type walkStepBudget struct {
	limit int
	used  int
	err   error
}

func (r *runJournal) chargeStep(name string, local int) error {
	limit, used := maxSteps, local
	if b := r.stepBudget; b != nil {
		if b.err != nil {
			return b.err
		}
		if b.limit > 0 {
			limit = b.limit
		}
		b.used++
		used = b.used
	}
	if used <= limit {
		return nil
	}
	err := fmt.Errorf("workflow %q exceeded max steps (%d): possible loop", name, limit)
	if r.stepBudget != nil {
		r.stepBudget.err = err
	}
	return err
}

func (r *runJournal) reportSteps(out *RunResult) {
	if r.branch == 0 && r.stepBudget != nil {
		out.Steps = r.stepBudget.used
	}
}
