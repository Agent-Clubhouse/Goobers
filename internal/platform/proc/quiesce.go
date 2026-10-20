package proc

import (
	"context"
	"errors"
	"time"
)

// ErrQuiescenceUnobservable means the host cannot attest workspace writer
// termination. Ordinary process cleanup may succeed, but capture must refuse.
var ErrQuiescenceUnobservable = errors.New("proc: workspace writer termination is unobservable")

// waitForQuiescence refuses unknown observations and cancellation. A successful
// kill request alone never satisfies the caller's custody proof.
func waitForQuiescence(ctx context.Context, empty func() (bool, error)) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(ErrQuiescenceUnobservable, err)
		}
		observed, err := empty()
		if err != nil {
			return errors.Join(ErrQuiescenceUnobservable, err)
		}
		if observed {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(ErrQuiescenceUnobservable, ctx.Err())
		case <-ticker.C:
		}
	}
}
