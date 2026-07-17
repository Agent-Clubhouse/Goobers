package invoke

import (
	"errors"
	"fmt"
	"testing"
)

func TestInfrastructureFailure(t *testing.T) {
	cause := errors.New("provider unavailable")
	err := InfrastructureFailure(cause)
	if !IsInfrastructureFailure(err) {
		t.Fatal("InfrastructureFailure did not apply its marker")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("marked error %q does not preserve cause %q", err, cause)
	}
	if !IsInfrastructureFailure(fmt.Errorf("dispatch: %w", err)) {
		t.Fatal("wrapped infrastructure marker was not detected")
	}
	if got := InfrastructureFailure(nil); got != nil {
		t.Fatalf("InfrastructureFailure(nil) = %v, want nil", got)
	}
	if IsInfrastructureFailure(cause) {
		t.Fatal("unmarked error classified as infrastructure")
	}
}

func TestTimeout(t *testing.T) {
	cause := errors.New("harness: session timed out")
	err := Timeout(cause)
	if !IsTimeout(err) {
		t.Fatal("Timeout did not apply its marker")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("marked error %q does not preserve cause %q", err, cause)
	}
	if !IsTimeout(fmt.Errorf("dispatch: %w", err)) {
		t.Fatal("wrapped timeout marker was not detected")
	}
	if got := Timeout(nil); got != nil {
		t.Fatalf("Timeout(nil) = %v, want nil", got)
	}
	if IsTimeout(cause) {
		t.Fatal("unmarked error classified as timeout")
	}
	// The two markers are independent: a timeout is not an infrastructure
	// failure, and vice versa — the runner routes them down different paths.
	if IsInfrastructureFailure(err) {
		t.Fatal("timeout marker misclassified as infrastructure")
	}
	if IsTimeout(InfrastructureFailure(cause)) {
		t.Fatal("infrastructure marker misclassified as timeout")
	}
}
