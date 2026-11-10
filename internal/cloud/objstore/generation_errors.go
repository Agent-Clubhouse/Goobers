package objstore

import "errors"

var (
	// ErrGenerationNotFound is a permanent missing-generation refusal.
	ErrGenerationNotFound = errors.New("cloud config generation not found")
	// ErrGenerationInvalid is a permanent corrupt or foreign-generation refusal.
	ErrGenerationInvalid = errors.New("cloud config generation is invalid")
	// ErrGenerationUnavailable is a transient failure to materialize a valid
	// generation locally (a full or failing temp volume, cancellation): the
	// archive is fine and a retry may succeed.
	ErrGenerationUnavailable = errors.New("cloud config generation is temporarily unavailable")
)
