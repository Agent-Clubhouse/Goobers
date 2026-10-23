package objstore

import "errors"

var (
	// ErrGenerationNotFound is a permanent missing-generation refusal.
	ErrGenerationNotFound = errors.New("cloud config generation not found")
	// ErrGenerationInvalid is a permanent corrupt or foreign-generation refusal.
	ErrGenerationInvalid = errors.New("cloud config generation is invalid")
)
