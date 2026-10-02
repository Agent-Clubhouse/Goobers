package main

import (
	"context"
	"io"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/nowork"
	"github.com/goobers/goobers/providers"
)

// withPriorNoWorkVerdict adds the item's recorded no-work verdict to a
// single claimed item's JSON. Best-effort by design: the verdict is advisory
// context, so a state-plane read failure is reported on stderr and the claim
// proceeds with the item exactly as it was, never failing the selection.
func withPriorNoWorkVerdict(
	ctx context.Context,
	l instance.Layout,
	repo providers.RepositoryRef,
	itemID string,
	data []byte,
	stderr io.Writer,
) []byte {
	record, err := loadNoWorkStreakRecord(ctx, l, repo, itemID)
	if err != nil {
		pf(stderr, "warning: read recorded no-work verdict for item %s: %v\n", itemID, err)
		return data
	}
	return nowork.WithPriorVerdict(data, record)
}
