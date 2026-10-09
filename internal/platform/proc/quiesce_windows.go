//go:build windows

package proc

import (
	"context"
	"fmt"
)

// StopAndWait remains unavailable until Job Object emptiness is verified after
// termination; ordinary Kill is deliberately not upgraded into custody proof.
func (t *Tree) StopAndWait(context.Context) error {
	return fmt.Errorf("workspace quiescence is not implemented for Windows: %w", ErrQuiescenceUnobservable)
}
