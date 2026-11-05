package runsemantics

import (
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestClassifyDispatchErrorInfrastructureWinsOverTimeout(t *testing.T) {
	// Reviewer session timeouts carry both markers (#5543); infrastructure
	// failure must win so a stalled provider does not consume policy retries.
	err := invoke.InfrastructureFailure(invoke.Timeout(errors.New("review session expired")))
	if !invoke.IsInfrastructureFailure(err) || !invoke.IsTimeout(err) {
		t.Fatal("fixture must carry both infrastructure and timeout markers")
	}
	code, class := ClassifyDispatchError(err)
	if code != telemetry.ErrCodeInfraFailure || class != telemetry.ErrorClassInfra {
		t.Fatalf("classification=(%q, %q), want infrastructure failure", code, class)
	}
}
