package localscheduler

import (
	"errors"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

func dispatchRunID(assigned string) (string, error) {
	if assigned == "" {
		return newRunID()
	}
	id, err := trace.TraceIDFromHex(assigned)
	if err != nil || !id.IsValid() || assigned != strings.ToLower(assigned) {
		return "", errors.New("assigned run ID must be a nonzero lowercase trace ID")
	}
	return assigned, nil
}
