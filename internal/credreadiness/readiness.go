// Package credreadiness classifies whether a configured credential source is
// usable under the identity that will actually execute a workflow.
//
// Source presence, token validity, and required-tool authorization are
// DIFFERENT observations, and conflating them is what makes an integration that
// an interactive user configured successfully fail under the service or worker
// a workflow actually runs on (#5261). A credential the operator can see in
// their own shell says nothing about what LocalSystem can read: a macOS
// Keychain item and a user-scoped CLI login are not reachable from a service
// account at all, so "the operator checked and it worked" is not evidence about
// the execution identity.
//
// Two rules keep this package from manufacturing the reassurance it exists to
// withhold:
//
//   - An unrecognized source kind is reported as unsupported, never silently
//     checked as an environment variable. Falling back to env is what turns an
//     unchecked credential into a clean bill of health.
//   - Anything that cannot be established under this identity -- an unreadable
//     keychain, an expiry the source does not state -- stays explicitly
//     unobservable. An explicit unknown is a usable answer; a plausible
//     substitute answers the question wrongly rather than declining to answer
//     it, which is the failure mode this whole package is aimed at.
//
// This is the 0.4.1 diagnostic slice. Acting on these observations -- running
// preflight under the execution identity rather than reporting on it -- is
// #4770, and nothing here presumes that design.
package credreadiness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
)

// SourceKind names where a credential comes from. It is reported so a reader
// can tell a machine-readable source (env, file, store) from one that is
// user-scoped by construction (keychain, CLI login) and therefore not
// necessarily reachable by a service account.
type SourceKind string

// The supported credential source kinds. SourceUnsupported is the fail-closed
// result for a ref this build cannot check, and is deliberately a value rather
// than an error: it has to be reportable alongside the kinds that did check.
const (
	SourceEnv         SourceKind = "env"
	SourceFile        SourceKind = "file"
	SourceKeychain    SourceKind = "keychain"
	SourceStore       SourceKind = "store"
	SourceGitHubCLI   SourceKind = "github_cli"
	SourceUnsupported SourceKind = "unsupported"
)

// UserScoped reports whether this source kind is bound to an interactive user
// account by construction. A user-scoped source that an operator can read in
// their own session is not thereby readable by a service identity, so a
// readiness claim about one says nothing about the other.
func (k SourceKind) UserScoped() bool {
	return k == SourceKeychain || k == SourceGitHubCLI
}

// Status is the stable result code for one check. Callers match on these
// values; they are part of the diagnostic contract and must not be reworded.
type Status string

const (
	// StatusUsable means the source resolved to a non-empty value under the
	// observing identity, and any expiry it stated is still in the future.
	StatusUsable Status = "usable"
	// StatusEmpty means the source resolved, but to nothing. This is
	// misconfiguration, not a valid empty secret.
	StatusEmpty Status = "empty"
	// StatusExpired means the source stated an expiry that has passed.
	StatusExpired Status = "expired"
	// StatusRefreshFailed means the source exists but would not yield a
	// current value -- a refresh that a headless identity cannot complete
	// because it needs an interactive sign-in is the motivating case.
	StatusRefreshFailed Status = "refresh_failed"
	// StatusUnobservable means the check could not be made under this
	// identity. It is NOT a failure and NOT a pass; it is the explicit
	// refusal to guess that the rest of this package exists to preserve.
	StatusUnobservable Status = "unobservable"
	// StatusUnsupportedSource means the ref names a source kind this build
	// cannot check. It fails closed here rather than degrading to an env
	// lookup that would report a clean result for an unchecked credential.
	StatusUnsupportedSource Status = "unsupported_source"
)

// Asserted reports whether this status is a positive readiness claim. Only
// StatusUsable is; everything else, including StatusUnobservable, is not.
func (s Status) Asserted() bool { return s == StatusUsable }

// Category separates the three observations #5261 requires not be conflated.
type Category string

const (
	// CategoryAuthentication covers whether a credential resolves at all.
	CategoryAuthentication Category = "authentication"
	// CategoryTransport covers reaching the service the credential is for.
	CategoryTransport Category = "transport"
	// CategoryToolAuthorization covers whether the authenticated identity is
	// permitted to use a required tool. A valid token that lacks a required
	// scope authenticates and transports fine and still cannot do the work.
	CategoryToolAuthorization Category = "tool_authorization"
)

// ErrUnobservable is returned by a resolver that cannot determine a credential's
// state under the current identity, as distinct from determining that it is
// unusable. Sources wrap it so classification never has to infer the difference
// from an error string.
var ErrUnobservable = errors.New("credreadiness: not observable under this identity")

// ErrRefreshFailed is returned when a source holds a credential it could not
// refresh -- typically because refreshing needs an interactive sign-in that a
// scheduled or service identity cannot perform.
var ErrRefreshFailed = errors.New("credreadiness: credential could not be refreshed")

// unknownIdentity is the explicit stand-in for an identity that could not be
// read. An empty string would be indistinguishable from "observed, and empty",
// and any plausible substitute would assert an identity that was never checked.
const unknownIdentity = "unknown"

// Observer is the identity a set of checks was made under. A readiness result
// belongs to the identity that produced it and does not transfer.
type Observer struct {
	// Account is the OS account the checking process is running as.
	Account string
	// Machine is the host the check ran on. A credential readable on one
	// machine is not thereby readable on another.
	Machine string
}

// CurrentObserver reports the identity of the calling process, degrading each
// field to an explicit unknown rather than omitting it.
func CurrentObserver() Observer {
	observer := Observer{Account: unknownIdentity, Machine: unknownIdentity}
	if account, err := user.Current(); err == nil && account.Username != "" {
		observer.Account = account.Username
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		observer.Machine = host
	}
	return observer
}

// Known reports whether both fields were actually observed. An observer with an
// unknown field cannot anchor a readiness claim, because there is no identity
// to attribute the claim to.
func (o Observer) Known() bool {
	return o.Account != "" && o.Account != unknownIdentity &&
		o.Machine != "" && o.Machine != unknownIdentity
}

func (o Observer) String() string { return o.Account + "@" + o.Machine }

// Check is one credential source's observed state. It names the source but
// never carries its value.
type Check struct {
	// Name is the logical credential name, e.g. "agent:model".
	Name string
	// Kind is where the credential comes from.
	Kind SourceKind
	// Source names the source WITHOUT its value: the env var's name, the
	// file's path, the store ref, the CLI host and login.
	Source string
	Status Status
	// Category says which of the three observations this check made.
	Category Category
	// Detail is a scrubbed, human-readable note. It is never required to
	// interpret Status, which is the machine-readable contract.
	Detail string
	// ExpiresAt is the expiry the source stated. ExpiryKnown distinguishes
	// "expires at the zero time" from "the source stated no expiry", which
	// must not be reported as unbounded validity.
	ExpiresAt   time.Time
	ExpiryKnown bool
}

// Report is a set of checks together with the identity that made them.
type Report struct {
	Observer  Observer
	CheckedAt time.Time
	Checks    []Check
}

// AssertsFor reports whether this report may be treated as evidence about
// other. It fails closed in both directions that matter: a report made under a
// different identity never transfers, and a report whose observer was not fully
// observed asserts nothing for anyone -- including for an identical-looking
// unknown, since two unknowns are not known to be the same identity.
func (r Report) AssertsFor(other Observer) bool {
	if !r.Observer.Known() || !other.Known() {
		return false
	}
	return r.Observer == other
}

// Ready reports whether every check positively asserted usability. An
// unobservable check makes a report not-ready: it is the absence of evidence,
// which is exactly what must not be rounded up to a pass.
func (r Report) Ready() bool {
	for _, check := range r.Checks {
		if !check.Status.Asserted() {
			return false
		}
	}
	return len(r.Checks) > 0
}

// Unready returns the checks that did not assert usability, sorted by name so
// output is stable.
func (r Report) Unready() []Check {
	var out []Check
	for _, check := range r.Checks {
		if !check.Status.Asserted() {
			out = append(out, check)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Describe reports a ref's source kind and its value-free source name.
//
// The switch is exhaustive over the source kinds credentials.TokenRef can
// carry, and a ref that matches none of them is SourceUnsupported. That
// refusal is the point: reporting an unrecognized ref as an env lookup is how
// an unchecked credential acquires a clean readiness result (#4816).
func Describe(ref credentials.TokenRef) (SourceKind, string) {
	switch {
	case ref.Env != "":
		return SourceEnv, ref.Env
	case ref.File != "":
		return SourceFile, ref.File
	case ref.Keychain != "":
		return SourceKeychain, ref.Keychain
	case ref.Store != "":
		return SourceStore, ref.Store
	case ref.GitHubCLI != nil:
		return SourceGitHubCLI, ref.GitHubCLI.Hostname + "/" + ref.GitHubCLI.User
	default:
		return SourceUnsupported, ""
	}
}

// ExpiringResolve resolves a credential and states its expiry. A zero expiry
// with a nil error means the source has no expiry to state, which is reported
// as an unknown expiry rather than as unbounded validity.
type ExpiringResolve func(ctx context.Context) (value string, expiresAt time.Time, err error)

// Probe observes one credential ref under the calling identity and classifies
// the result. resolve may be nil, which is itself an observation: a ref with no
// resolver cannot be checked, so it is unobservable rather than absent.
//
// The resolved value is used only to establish presence and is never retained:
// it does not reach the returned Check, and every error is scrubbed before it
// does.
func Probe(ctx context.Context, name string, ref credentials.TokenRef, resolve ExpiringResolve, scrubber journal.Scrubber, now time.Time) Check {
	kind, source := Describe(ref)
	check := Check{
		Name:     name,
		Kind:     kind,
		Source:   source,
		Category: CategoryAuthentication,
	}
	if kind == SourceUnsupported {
		check.Status = StatusUnsupportedSource
		check.Detail = "ref names no supported credential source; not checked as an environment variable"
		return check
	}
	if resolve == nil {
		check.Status = StatusUnobservable
		check.Detail = "no resolver is wired for this source"
		return check
	}

	value, expiresAt, err := resolve(ctx)
	check.ExpiresAt = expiresAt
	check.ExpiryKnown = !expiresAt.IsZero()
	switch {
	case errors.Is(err, ErrUnobservable):
		check.Status = StatusUnobservable
	case errors.Is(err, ErrRefreshFailed):
		check.Status = StatusRefreshFailed
	case errors.Is(err, credentials.ErrTokenRefEmpty):
		check.Status = StatusEmpty
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// A check that ran out of time did not establish anything. Reporting
		// it as a failure would be as wrong as reporting it as a pass.
		check.Status = StatusUnobservable
	case err != nil:
		check.Status = StatusRefreshFailed
	case strings.TrimSpace(value) == "":
		check.Status = StatusEmpty
	case check.ExpiryKnown && !expiresAt.After(now):
		check.Status = StatusExpired
	default:
		check.Status = StatusUsable
	}
	if err != nil {
		check.Detail = scrub(scrubber, err.Error())
	} else if check.Status == StatusUsable && !check.ExpiryKnown {
		// A source that states no expiry has not said the credential is
		// long-lived; it has said nothing. Record that explicitly so a reader
		// does not take silence for a guarantee.
		check.Detail = "source states no expiry; validity window unknown"
	}
	return check
}

// scrub redacts a probe detail before it can reach JSON or human output. A
// resolver error can quote the value it failed on, so this runs on every
// detail rather than on the ones judged risky.
func scrub(scrubber journal.Scrubber, detail string) string {
	if scrubber == nil {
		return detail
	}
	return string(scrubber.Scrub([]byte(detail)))
}

// Summarize renders one check as a single stable line for human output.
func (c Check) Summarize() string {
	descriptor := fmt.Sprintf("source %s %s", c.Kind, c.Source)
	if c.Source == "" {
		// A kind with no source name is not a source with an unknown name --
		// there is no source. Saying "unknown" would imply one exists.
		descriptor = "no credential source"
	}
	line := fmt.Sprintf("%s: %s (%s)", c.Name, c.Status, descriptor)
	if c.Detail != "" {
		line += ": " + c.Detail
	}
	return line
}
