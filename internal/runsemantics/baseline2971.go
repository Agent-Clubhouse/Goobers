package runsemantics

// SharedBaselineFailureCode is the stage-failure code a local-ci failure
// carries once it is attributed to the target branch rather than to the run's
// own diff (#2971). It is a recognized non-retryable disposition
// (escalateErrorCodes), so the affected run parks against the shared blocker
// through the existing #415 route instead of spending an implementation repass
// re-deriving that there is nothing branch-local to fix.
const SharedBaselineFailureCode = "SHARED_BASELINE_FAILURE"
