// Package workspacedelta is the ONE implementation of the mode-3 workspace
// continuity carrier (#3763, #3803, #3767): a thin git bundle of
// base..<tip> that travels by content digest through the blob plane, and
// the ancestry-guarded reconciliation that lands it on a receiving ref.
//
// WHY ONE PACKAGE. The carrier crosses three substrates — a stage pod's
// checkout (cmd/goobers dispatch-exec), the worker's managed mirror
// (internal/worktree) and, per delivery decision 003 ruling 5, the daemon's
// credentialed mirror. #3821 landed the ancestry guard on the pod side alone;
// a second copy on the worker side would be the exact "two implementations of
// one convention" drift this repository keeps paying for. The git mechanics
// (which ref the bundle names, how the fetched tip is classified against the
// receiving ref) live here once; each caller supplies only its own way of
// running git (Git), because the environments genuinely differ — the pod needs
// the safe.directory exemption on every call, the mirror needs the hardened
// -c overrides.
//
// THE GUARD (#3821, kept byte-for-byte in spirit): a delta must never rewind
// a receiving ref that has already moved past it. Self-placed stages and
// provider-side producers (update-behind-pr) can advance a branch without
// the record knowing, so the digest a consumer is handed can be STALE. The
// arms, in order:
//
//   - receiving ref absent                 -> create it at the tip;
//   - ref is an ancestor of the tip        -> fast-forward (equal counts);
//   - tip is an ancestor of the ref        -> keep the ref (it already
//     carries everything the delta does) and say so with both SHAs;
//   - neither, but the ref is itself an ancestor of the CURRENT base ->
//     base drift, not divergence (the base moved between two checkouts and
//     the ref carries nothing but base) -> apply exactly as fast-forward;
//   - neither                              -> DivergedError naming both SHAs.
//     No merge, no rebase: inventing history nobody in the run asked for is
//     worse than failing the stage.
package workspacedelta

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Ref is the private ref name every bundle carries its commits under. A fixed
// name rather than the run branch: the receiving side fetches by this name
// and never has to agree with the sender about branch naming, and it cannot
// collide with a real branch in the bundle's namespace.
const Ref = "refs/goobers/workspace-delta"

// Git runs git for one caller. Run executes a command for its side effect;
// Output returns trimmed stdout. Both must surface git's exit status through
// the returned error so exit-1 "no" answers (merge-base --is-ancestor) stay
// distinguishable from real failures: wrap an *exec.ExitError, do not
// flatten it.
type Git interface {
	Run(ctx context.Context, dir string, args ...string) error
	Output(ctx context.Context, dir string, args ...string) (string, error)
}

// Bundle is one published delta: the bytes, their content address, and the
// two SHAs that describe what the bytes carry — the prerequisite (Base) the
// receiving side must already hold and the head (Tip) the delta lands on.
// Base/Tip are what the engine journals; the bytes never enter history.
type Bundle struct {
	Digest string
	Data   []byte
	Base   string
	Tip    string
}

// Digest is the content address of data in the blob plane's own spelling
// (sha256:<hex>), identical to agentickit.Digest and journal.Digest.
// Restated here rather than imported so this leaf package stays free of the
// journal and kit dependencies internal/worktree deliberately does not carry.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Verify checks that data hashes to digest — done on arrival by every
// receiver and for the same reason the agentic kit does it: a substituted
// delta means running a stage on top of commits nobody in this run made.
func Verify(data []byte, digest string) error {
	if got := Digest(data); got != digest {
		return fmt.Errorf("workspace delta digest mismatch: got %s, expected %s", got, digest)
	}
	return nil
}

// Load wraps bytes fetched from a store as a Bundle after verifying them.
// Base/Tip are unknown until the bundle is fetched into a repository.
func Load(data []byte, digest string) (Bundle, error) {
	if err := Verify(data, digest); err != nil {
		return Bundle{}, err
	}
	return Bundle{Digest: digest, Data: data}, nil
}

// Create bundles baseRef..tipRef from the repository at dir. baseRef and
// tipRef are anything rev-parse resolves (a branch, "HEAD", a remote-tracking
// ref). The bundle NAMES Ref, never tipRef: `git bundle create <f>
// <base>..<branch>` records refs/heads/<branch>, which would force both sides
// to agree on branch naming; pointing the fixed private ref at the tip first
// makes the carrier self-describing. The ref is removed again before
// returning, so the source repository is left exactly as found.
//
// A THIN bundle carries only the commits beyond baseRef and needs the base
// object present on the receiving side. If the receiver's base has moved
// under it git refuses the fetch with "does not contain prerequisite
// commits" — loud and named, the right failure for a carrier of real work.
func Create(ctx context.Context, git Git, dir, baseRef, tipRef string) (Bundle, error) {
	base, err := git.Output(ctx, dir, "rev-parse", "--verify", baseRef+"^{commit}")
	if err != nil {
		return Bundle{}, fmt.Errorf("workspace delta: resolve base %s: %w", baseRef, err)
	}
	if err := git.Run(ctx, dir, "update-ref", Ref, tipRef); err != nil {
		return Bundle{}, fmt.Errorf("workspace delta: name delta ref: %w", err)
	}
	defer func() { _ = git.Run(context.WithoutCancel(ctx), dir, "update-ref", "-d", Ref) }()
	tip, err := git.Output(ctx, dir, "rev-parse", "--verify", Ref+"^{commit}")
	if err != nil {
		return Bundle{}, fmt.Errorf("workspace delta: resolve tip %s: %w", tipRef, err)
	}

	// The bundle is written OUTSIDE the repository: a stray file inside it
	// would show up as an untracked change to whatever runs next.
	tmp, err := os.MkdirTemp("", "goobers-delta-")
	if err != nil {
		return Bundle{}, fmt.Errorf("workspace delta: create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	path := filepath.Join(tmp, "workspace.bundle")
	if err := git.Run(ctx, dir, "bundle", "create", path, baseRef+".."+Ref); err != nil {
		return Bundle{}, fmt.Errorf("workspace delta: bundle %s..%s: %w", baseRef, tipRef, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Bundle{}, fmt.Errorf("workspace delta: read bundle: %w", err)
	}
	return Bundle{Digest: Digest(data), Data: data, Base: strings.TrimSpace(base), Tip: strings.TrimSpace(tip)}, nil
}

// Fetch verifies b and fetches its Ref into the repository at dir, returning
// the SHA the bundle carried (FETCH_HEAD). It moves no ref of the receiver's
// own: Reconcile decides what happens next, and the caller acts on that.
func Fetch(ctx context.Context, git Git, dir string, b Bundle) (string, error) {
	if err := Verify(b.Data, b.Digest); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "goobers-delta-")
	if err != nil {
		return "", fmt.Errorf("workspace delta: create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	path := filepath.Join(tmp, "workspace.bundle")
	if err := os.WriteFile(path, b.Data, 0o600); err != nil {
		return "", fmt.Errorf("workspace delta: write bundle: %w", err)
	}
	if err := git.Run(ctx, dir, "fetch", "--quiet", path, Ref); err != nil {
		return "", fmt.Errorf("apply workspace delta %s: %w", b.Digest, err)
	}
	tip, err := git.Output(ctx, dir, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("workspace delta %s: determine fetched tip: %w", b.Digest, err)
	}
	return strings.TrimSpace(tip), nil
}

// Outcome is Reconcile's verdict on how the receiving ref relates to the
// fetched tip.
type Outcome int

const (
	// OutcomeCreate means the receiving ref does not exist; create it at the tip.
	OutcomeCreate Outcome = iota + 1
	// OutcomeFastForward means the ref is an ancestor of (or equal to) the tip;
	// move it there.
	OutcomeFastForward
	// OutcomeKeep means the tip is strictly behind the ref — the ref already
	// carries everything the delta does. Leave it alone.
	OutcomeKeep
	// OutcomeBaseDrift means neither contains the other, but the ref is nothing
	// more than an advanced base. Apply the delta exactly as a fast-forward.
	OutcomeBaseDrift
)

// String names the outcome for logs and journals.
func (o Outcome) String() string {
	switch o {
	case OutcomeCreate:
		return "create"
	case OutcomeFastForward:
		return "fast-forward"
	case OutcomeKeep:
		return "keep"
	case OutcomeBaseDrift:
		return "base-drift"
	}
	return fmt.Sprintf("outcome(%d)", int(o))
}

// DivergedError is the fail-closed arm: the receiving ref and the delta each
// carry real commits the other lacks. Both SHAs are named so the far-side
// record (pod stderr, worker log, surrendered error) can be read after the
// process is gone.
type DivergedError struct {
	Digest  string
	Current string
	Tip     string
}

func (e *DivergedError) Error() string {
	return fmt.Sprintf("workspace delta %s has diverged from the receiving ref: ref is at %s, delta carries %s (neither is an ancestor of the other); refusing to overwrite history", e.Digest, e.Current, e.Tip)
}

// Reconcile classifies current — the SHA the receiving ref points at, or ""
// when the ref does not exist — against tip, the SHA Fetch returned. baseRef
// names the CURRENT base in the receiving repository (origin/<base> in a
// checkout, refs/heads/<base> in a mirror) for the base-drift arm; empty
// disables that arm. A failure resolving baseRef is not treated as "assume
// drift": refusing to guess and falling through to DivergedError is the safe
// direction. See the package comment for the arms.
func Reconcile(ctx context.Context, git Git, dir, digest, current, tip, baseRef string) (Outcome, error) {
	if current == "" {
		return OutcomeCreate, nil
	}
	fastForward, err := IsAncestor(ctx, git, dir, current, tip)
	if err != nil {
		return 0, fmt.Errorf("workspace delta %s: check whether %s is an ancestor of %s: %w", digest, current, tip, err)
	}
	if fastForward {
		return OutcomeFastForward, nil
	}
	behind, err := IsAncestor(ctx, git, dir, tip, current)
	if err != nil {
		return 0, fmt.Errorf("workspace delta %s: check whether %s is an ancestor of %s: %w", digest, tip, current, err)
	}
	if behind {
		return OutcomeKeep, nil
	}
	if baseRef != "" {
		if _, resolveErr := git.Output(ctx, dir, "rev-parse", "--verify", baseRef+"^{commit}"); resolveErr == nil {
			driftOnly, err := IsAncestor(ctx, git, dir, current, baseRef)
			if err != nil {
				return 0, fmt.Errorf("workspace delta %s: check whether ref %s is base drift (ancestor of %s): %w", digest, current, baseRef, err)
			}
			if driftOnly {
				return OutcomeBaseDrift, nil
			}
		}
	}
	return 0, &DivergedError{Digest: digest, Current: current, Tip: tip}
}

// IsAncestor reports whether ancestor is reachable from descendant — a commit
// counts as its own ancestor, matching `git merge-base --is-ancestor`. Exit 1
// is the ordinary "no"; anything else (128: bad object, corrupt repo) is a
// real failure and is returned as one.
func IsAncestor(ctx context.Context, git Git, dir, ancestor, descendant string) (bool, error) {
	err := git.Run(ctx, dir, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("merge-base --is-ancestor %s %s: %w", ancestor, descendant, err)
}
