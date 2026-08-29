package executor

// StageError carries a stage failure's machine-readable code across the
// invoke seam. The executor already knows that code at the moment it gives
// up — a provider builtin's self-reported errorCode (#614), a claims-lock
// timeout, a stage timeout — but a RETRYABLE failure is returned as a plain
// error wrapped for infrastructure retry, so until this type the code
// survived only inside the human message. Every such attempt then journaled
// as the opaque executor_error, which is how one gaggle's clone-403, askpass,
// DNS and rate-limit regimes ended up sharing a single unqueryable bucket
// (2026-08-08 reliability audit).
//
// The code is exposed as a METHOD, not a field, so internal/runner can match
// it with errors.As against its own one-method interface: the runner core
// stays dependent on the invoke seam alone and never imports this package.
type StageError struct {
	code string
	err  error
}

func (e *StageError) Error() string { return e.err.Error() }

func (e *StageError) Unwrap() error { return e.err }

// StageErrorCode is the stage failure's machine-readable code.
func (e *StageError) StageErrorCode() string { return e.code }

// StageFailure tags err with a machine-readable code, preserving err's
// message verbatim. A nil err or empty code returns err unchanged, so call
// sites need no guard.
func StageFailure(code string, err error) error {
	if err == nil || code == "" {
		return err
	}
	return &StageError{code: code, err: err}
}
