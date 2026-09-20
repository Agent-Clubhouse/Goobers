package gate

import "github.com/goobers/goobers/internal/runcontrol"

// RepassBudget preserves the runtime contract while sharing pure budget
// arithmetic with advisory analysis without importing execution adapters.
type RepassBudget = runcontrol.RepassBudget

// RepassCharge reports a transition from the shared runtime budget.
type RepassCharge = runcontrol.RepassCharge
