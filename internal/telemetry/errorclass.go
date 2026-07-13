package telemetry

import "strings"

// ErrorClass is a normalized, queryable error category (TEL-012/#22) so the
// work-nomination workflow can query failure patterns across runs without
// parsing free-text messages. It is derived from the journal error event's
// stable machine-readable Code (internal/journal.ErrorDetail.Code) via
// ClassifyError — this package never imports internal/journal (decoupled, same
// playbook as #12's provider seams); it only documents the code convention a
// runner/provider-adapter should follow.
type ErrorClass string

// The error taxonomy. Kept small and stable: broad enough for nomination to
// query meaningfully, narrow enough that every runner-emitted code has an
// unambiguous home.
const (
	ErrorClassHarnessFailure    ErrorClass = "harness-failure"
	ErrorClassProviderRateLimit ErrorClass = "provider-rate-limit"
	ErrorClassTimeout           ErrorClass = "timeout"
	ErrorClassValidation        ErrorClass = "validation"
	ErrorClassInfra             ErrorClass = "infra"
	ErrorClassUnknown           ErrorClass = "unknown"
)

// Well-known error codes. These are the exact internal/journal.ErrorDetail.Code
// values a runner or provider adapter should emit so ClassifyError resolves
// them without falling back to heuristics. ErrCodeProviderRateLimit in
// particular is the code the runner should use when adapting a
// providers.RateLimitObserver event (#12) into a journal error event / span
// event — keeping the two mission's telemetry consistent.
const (
	ErrCodeProviderRateLimit = "provider.rate_limit"
	ErrCodeTimeout           = "timeout"
	ErrCodeHarnessFailure    = "harness.failure"
	ErrCodeValidationFailed  = "validation.failed"
	ErrCodeInfraFailure      = "infra.failure"
)

var wellKnownErrorCodes = map[string]ErrorClass{
	ErrCodeProviderRateLimit: ErrorClassProviderRateLimit,
	ErrCodeTimeout:           ErrorClassTimeout,
	ErrCodeHarnessFailure:    ErrorClassHarnessFailure,
	ErrCodeValidationFailed:  ErrorClassValidation,
	ErrCodeInfraFailure:      ErrorClassInfra,
}

// ClassifyError normalizes a journal error event's Code into an ErrorClass.
// Empty code classifies as empty (no error). An exact well-known code always
// wins; otherwise a small set of prefix/substring heuristics covers codes that
// follow the documented dotted-namespace convention without matching exactly.
// Anything else is ErrorClassUnknown rather than guessed at, so nomination
// queries can distinguish "known-unclassifiable" from "not yet seen."
func ClassifyError(code string) ErrorClass {
	if code == "" {
		return ""
	}
	if class, ok := wellKnownErrorCodes[code]; ok {
		return class
	}
	lower := strings.ToLower(code)
	switch {
	case strings.Contains(lower, "rate_limit"), strings.Contains(lower, "rate-limit"):
		return ErrorClassProviderRateLimit
	case strings.Contains(lower, "timeout"):
		return ErrorClassTimeout
	case strings.HasPrefix(lower, "harness."):
		return ErrorClassHarnessFailure
	case strings.HasPrefix(lower, "validation."), strings.HasPrefix(lower, "schema."):
		return ErrorClassValidation
	case strings.HasPrefix(lower, "infra."):
		return ErrorClassInfra
	default:
		return ErrorClassUnknown
	}
}
