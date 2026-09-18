package recovery

import (
	"context"
	"fmt"
	"io"
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
// repository may be a bare repository: only the object store is consulted, so
// this can answer before any working tree exists.
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

// CommitContainedIn reports whether commit is already reachable from baseRef,
// i.e. the base branch carries everything the commit does. Terminal capture
// uses it to decide, from a bare mirror and before materializing any checkout,
// whether a run branch is worth capturing at all.
//
// A base ref that cannot be resolved reports false: "I could not establish that
// this work is already safe" must lead to capturing it, not to skipping it.
// The caller's SkipEmpty still drops a capture that turns out to carry nothing.
func CommitContainedIn(ctx context.Context, repository, commit, baseRef string) (bool, error) {
	if !gitObjectID.MatchString(commit) {
		return false, fmt.Errorf("recovery containment check requires a commit identity")
	}
	if baseRef == "" || strings.HasPrefix(baseRef, "-") || strings.ContainsAny(baseRef, "\x00\r\n") {
		return false, fmt.Errorf("invalid recovery containment base ref")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var base boundedRefOutput
	if err := recoveryGit(ctx, repository, &base, "rev-parse", "--verify", "--quiet", baseRef+"^{commit}"); err != nil {
		return false, nil
	}
	baseSHA := strings.TrimSpace(base.String())
	if !gitObjectID.MatchString(baseSHA) {
		return false, nil
	}
	if baseSHA == commit {
		return true, nil
	}
	// merge-base --is-ancestor exits 0 for "contained", 1 for "not", and
	// anything else for a real failure. Only exit 0 may suppress a capture.
	err := recoveryGit(ctx, repository, io.Discard, "merge-base", "--is-ancestor", commit, baseSHA)
	return err == nil, nil
}
