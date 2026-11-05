// Package journalequivalence checks the shared local/cloud journal corpus.
package journalequivalence

import (
	"context"
	"slices"

	"github.com/goobers/goobers/test/ha/chaos"
)

// PendingFixtures is the complete known-divergence registry. These fixtures
// still run and pin their failure mode; they do not claim full equivalence.
// Closing the issue makes the hosted verify-pending check fail until the
// regression is promoted to full equivalence.
func PendingFixtures() map[string]string {
	return map[string]string{}
}

// VerifyPending uses the same live GitHub issue-state check as HA chaos.
func VerifyPending(ctx context.Context, exec chaos.Executor) error {
	return verifyPendingFixtures(ctx, exec, PendingFixtures())
}

func verifyPendingFixtures(ctx context.Context, exec chaos.Executor, fixtures map[string]string) error {
	var refs []string
	for _, ref := range fixtures {
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	return chaos.VerifyPendingOpen(ctx, exec, refs)
}
