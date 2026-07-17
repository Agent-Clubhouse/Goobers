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
