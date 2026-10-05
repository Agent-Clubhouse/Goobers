package sessioning

import "errors"

// Service errors are neutral so HTTP/CLI adapters need not import the runtime.
var (
	ErrInvalidRequest = errors.New("session request is invalid")
	ErrDenied         = errors.New("session operation is not authorized")
	ErrNotFound       = errors.New("session was not found")
	ErrConflict       = errors.New("session request key belongs to different input")
	ErrCapacity       = errors.New("session capacity is exhausted")
	ErrClosed         = errors.New("session intake is closed")
	ErrExpired        = errors.New("session request receipt has expired")
	ErrUnavailable    = errors.New("session runtime is unavailable")
)
