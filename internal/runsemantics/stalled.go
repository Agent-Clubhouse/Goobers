package runsemantics

// RunStalledErrorCode identifies watchdog terminalizations in run journals.
const RunStalledErrorCode = "run_stalled"

// RunCanceledErrorCode identifies operator-requested live cancellations
// (`goobers run cancel`, #831) in run journals, distinct from the watchdog's
// stall terminalizations (RunStalledErrorCode).
const RunCanceledErrorCode = "run_canceled"

// RunDurationExceededErrorCode identifies watchdog cancellations caused by a
// run exceeding its configured total wall-clock duration.
const RunDurationExceededErrorCode = "run_duration_exceeded"

// StageInterruptedErrorCode identifies operator-requested interrupts of a
// single running agentic stage (#1995) in run journals. Unlike a cancel
// (RunCanceledErrorCode), which terminalizes the whole run aborted, a stage
// interrupt terminalizes the run escalated — a recoverable phase from which
// RerunStage continues the interrupted stage with an operator correction
// folded into the envelope. It is distinct from the watchdog's stall
// terminalization (RunStalledErrorCode): the trigger is a human, not the
// no-progress watchdog.
const StageInterruptedErrorCode = "stage_interrupted"
