package main

import (
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry/retention"
)

// Applies even before a parent has accepted its first child and has no queue
// family row. Journal deletion would otherwise erase the only fork archive.
func guardParentContributionPrune(candidate retention.Result) error {
	reader, err := journal.OpenReadOnly(candidate.RunDir)
	if err != nil {
		return err
	}
	return requireRetiredParentContributions(reader)
}

func requireRetiredParentContributions(reader *journal.Reader) error {
	return runner.RequireRetiredParentContributions(reader)
}
