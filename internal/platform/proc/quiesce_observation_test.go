package proc

import (
	"context"
	"errors"
	"testing"
)

func TestQuiescenceRequiresObservedEmptyCustody(t *testing.T) {
	calls := 0
	err := waitForQuiescence(t.Context(), func() (bool, error) { calls++; return calls == 3, nil })
	if err != nil || calls != 3 {
		t.Fatalf("proof before empty observation: calls=%d err=%v", calls, err)
	}
}

func TestQuiescenceRefusesUnknownObservation(t *testing.T) {
	failure := errors.New("accounting unavailable")
	err := waitForQuiescence(t.Context(), func() (bool, error) { return true, failure })
	if !errors.Is(err, ErrQuiescenceUnobservable) || !errors.Is(err, failure) {
		t.Fatalf("unknown observation accepted: %v", err)
	}
}

func TestQuiescenceCancellationCannotBecomeProof(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	err := waitForQuiescence(ctx, func() (bool, error) { calls++; cancel(); return false, nil })
	if !errors.Is(err, ErrQuiescenceUnobservable) || !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancelled custody: calls=%d err=%v", calls, err)
	}
}
