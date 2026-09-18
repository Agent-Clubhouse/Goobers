package recovery

import (
	"context"
	"fmt"
	"strings"
)

// SnapshotCoversCommit reports whether an already-retained snapshot was
// captured from a checkout whose HEAD was exactly commit.
//
// CaptureSnapshot commits the captured tree with the checkout's HEAD as its
// single parent, so the parent of a record's SnapshotSHA IS the commit that
// capture protected. Reading one parent is the cheapest correct check: it is a
// single rev-parse against an object the same repository already holds, and it
// never depends on the snapshot's own identity timestamp — which a stage
// capture and a terminal capture deliberately do not share, so comparing
// snapshot SHAs directly would report "not covered" for a snapshot that
// protects the identical work.
//
// Comparing trees instead would be equally correct but strictly more work (two
// rev-parses and a resolution of the commit's own tree) for an answer that
// differs only when a snapshot's tree matches the commit while its parent does
// not — which, for a clean checkout, is the same case seen from the other side.
//
// The snapshot object must be present locally. An absent or unreadable object
// is reported as NOT covering: a caller deciding whether work still needs
// capturing must not read "I cannot see the evidence" as "the work is safe".
func SnapshotCoversCommit(ctx context.Context, repository, snapshotSHA, commit string) (bool, error) {
	if !gitObjectID.MatchString(snapshotSHA) || !gitObjectID.MatchString(commit) {
		return false, fmt.Errorf("recovery coverage check requires two object identities")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var parent boundedRefOutput
	if err := recoveryGit(ctx, repository, &parent, "rev-parse", "--verify", "--quiet", snapshotSHA+"^1^{commit}"); err != nil {
		return false, nil
	}
	return strings.TrimSpace(parent.String()) == commit, nil
}
