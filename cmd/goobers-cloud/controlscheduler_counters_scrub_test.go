package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestCounterReportsAreScrubbedOfRegisteredSecrets(t *testing.T) {
	scrubber := journal.NewRegistryScrubber()
	scrubber.Register([]byte("ghs_secrettoken123"))
	var got []error
	report := scrubbedReport(scrubber, func(err error) { got = append(got, err) })
	report(errors.New("poll github: Authorization ghs_secrettoken123 rejected"))
	report(nil)
	if len(got) != 2 || got[1] != nil {
		t.Fatalf("reports = %v, want one scrubbed error then nil", got)
	}
	if strings.Contains(got[0].Error(), "ghs_secrettoken123") {
		t.Fatalf("report leaked the registered secret: %q", got[0])
	}
}
