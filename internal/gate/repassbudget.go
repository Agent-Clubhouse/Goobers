package gate

import "github.com/goobers/goobers/internal/runcontrol"

// Keep the runtime contract while allowing advisory analysis to share the pure
// budget arithmetic without importing agent harnesses or execution adapters.
type RepassBudget = runcontrol.RepassBudget
type RepassCharge = runcontrol.RepassCharge
