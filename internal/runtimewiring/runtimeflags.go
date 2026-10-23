package runtimewiring

import (
	"errors"
	"strings"

	"github.com/goobers/goobers/internal/journal"
)

// WorkerDivergenceAppender records configuration divergence events.
type WorkerDivergenceAppender interface{ Append(journal.Event) error }

// RepeatableFlag collects repeated CLI option values.
type RepeatableFlag []string

func (f *RepeatableFlag) String() string { return strings.Join(*f, ",") }

// Set appends one option value.
func (f *RepeatableFlag) Set(value string) error {
	if value == "" {
		return errors.New("value must be non-empty")
	}
	*f = append(*f, value)
	return nil
}
