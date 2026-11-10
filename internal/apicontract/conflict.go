package apicontract

import "errors"

// ErrWriteConflict means an immutable identity already has different content.
var ErrWriteConflict = errors.New("control-plane write conflicts with existing content")
