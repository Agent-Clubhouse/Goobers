package credentials

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrPreflight is the sentinel every credential preflight failure matches, so
// callers can distinguish "the instance is misconfigured" from a transient
// resolve error encountered mid-run.
var ErrPreflight = errors.New("credentials: preflight failed")

// RefProblem is one configured secret reference that did not resolve at
// startup. It names the ref and where the value was expected to come from, and
// never carries secret material — Err comes from the resolve path, which
// reports only the locator (env var name, file path), never the value.
type RefProblem struct {
	// Ref is the logical token ref name, e.g. "goobers-io/goobers".
	Ref string
	// Source is the ref's source kind: "env" or "file".
	Source string
	// Locator is the env var name or file path the value was read from.
	Locator string
	// Err is the underlying resolve failure.
	Err error
}

// PreflightError aggregates every unresolvable reference found at startup.
// All problems are reported together so an operator fixes the whole
// environment in one pass instead of restarting once per missing variable.
type PreflightError struct {
	// Problems is every reference that failed, ordered by ref name.
	Problems []RefProblem
	// Checked is how many references were preflighted.
	Checked int
}

func (e *PreflightError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d of %d configured secret reference(s) did not resolve", ErrPreflight.Error(), len(e.Problems), e.Checked)
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "\n  - ref %q (%s %s): %v", p.Ref, p.Source, p.Locator, p.Err)
	}
	b.WriteString("\n\nThese are read from the daemon's own process environment and filesystem." +
		" A daemon started by a service manager, or respawned by self-update, does not inherit variables" +
		" exported in an operator's interactive shell — set them where the daemon itself is launched." +
		" Startup fails closed here rather than several stages into a run, so no worktree, claim, or" +
		" run budget is consumed by a credential the instance never had.")
	return b.String()
}

// Is reports ErrPreflight so errors.Is(err, ErrPreflight) matches.
func (e *PreflightError) Is(target error) bool { return target == ErrPreflight }

// Unwrap exposes the underlying resolve failures to errors.Is/As.
func (e *PreflightError) Unwrap() []error {
	errs := make([]error, 0, len(e.Problems))
	for _, p := range e.Problems {
		errs = append(errs, p.Err)
	}
	return errs
}

// Preflighter verifies that every locally-readable secret reference resolves,
// without exposing any value. Implemented by the resolver this package builds;
// callers type-assert so a Resolver from another source stays usable.
type Preflighter interface {
	Preflight(ctx context.Context) error
}

var _ Preflighter = (*tokenRefResolver)(nil)

// preflightable reports whether a ref can be verified at startup for free.
//
// Env and file refs are local, deterministic, and cost nothing to read — and
// they are the refs that actually go missing, because they depend on the
// daemon's ambient process environment rather than on instance.yaml alone.
//
// Keychain, store-backed, and dynamic (minting) sources are deliberately
// excluded: they prompt, call the network, or consume a provider rate budget,
// so eagerly exercising them at every startup would trade one failure mode for
// another. Those remain resolved on first use.
func preflightable(r TokenRef) (source, locator string, ok bool) {
	switch {
	case r.Env != "":
		return "env", r.Env, true
	case r.File != "":
		return "file", r.File, true
	default:
		return "", "", false
	}
}

// Preflight resolves every env- and file-backed token ref and reports all
// failures together. It returns nil when there is nothing to check, so an
// instance with no static credentials starts unchanged.
func (r *tokenRefResolver) Preflight(ctx context.Context) error {
	names := make([]string, 0, len(r.refs))
	for name := range r.refs {
		names = append(names, name)
	}
	sort.Strings(names)

	var problems []RefProblem
	checked := 0
	for _, name := range names {
		ref := r.refs[name]
		source, locator, ok := preflightable(ref)
		if !ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		checked++
		if _, err := ref.resolve(ctx, r.stores); err != nil {
			problems = append(problems, RefProblem{Ref: name, Source: source, Locator: locator, Err: err})
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return &PreflightError{Problems: problems, Checked: checked}
}
