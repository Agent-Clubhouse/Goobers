// Package claimrelease defines why a backlog claim ended (#4421).
//
// A claim release used to carry only the releasing run's id: the ledger's
// history, the instance journal's claim.released event and the provider's
// goobers-claim-release comment all said THAT ownership ended, never WHY, so a
// repeatedly claimed and released item read as unexplained flapping. A Reason
// is the one structured disposition every layer records — the ledger keeps it
// on the released history entry, the journal on the release event itself, and
// the provider comment renders it for humans next to a versioned
// machine-readable marker line.
//
// The package is a leaf (it depends only on the secret-shape net and the
// byte-bounded truncator) so the ledger, the journal envelope, the claims
// plane's wire types and the providers can all share one definition.
package claimrelease

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/goobers/goobers/internal/secretpattern"
	"github.com/goobers/goobers/internal/truncate"
)

// Disposition is the stable, machine-readable class of a claim release.
type Disposition string

// Release dispositions. The string values are a persisted contract (ledger,
// journal, provider marker): add new ones, never rename.
const (
	// Completed is a run that finished its work on the item.
	Completed Disposition = "completed"
	// Failed is a run that ended unsuccessfully.
	Failed Disposition = "failed"
	// Escalated is a run that ended needing human attention or remediation.
	Escalated Disposition = "escalated"
	// Cancelled is an operator abort, drain or signal that ended the run.
	Cancelled Disposition = "cancelled"
	// Abandoned is a run that gave the item back without finishing it.
	Abandoned Disposition = "abandoned"
	// NoWork is a run that found nothing to do for the item.
	NoWork Disposition = "no-work"
	// StallSwept is a lease that expired without renewal (a stalled or crashed
	// holder) and was reclaimed by the recovery sweep.
	StallSwept Disposition = "stall-swept"
	// Reconciled is marker or orphan cleanup that repairs drift between the
	// provider-visible claim and the authoritative ledger.
	Reconciled Disposition = "reconciled"
	// ForceReleased is an operator overriding a lease.
	ForceReleased Disposition = "force-released"
	// Superseded is a release because another run or owner now holds the item.
	Superseded Disposition = "superseded"
	// Unknown is a release whose reason was not recorded: a legacy caller, a
	// legacy ledger entry, or a path that cannot recover its cause.
	Unknown Disposition = "unknown"
)

var known = map[Disposition]struct{}{
	Completed: {}, Failed: {}, Escalated: {}, Cancelled: {}, Abandoned: {}, NoWork: {},
	StallSwept: {}, Reconciled: {}, ForceReleased: {}, Superseded: {}, Unknown: {},
}

// Known reports whether d is one of this package's dispositions.
func (d Disposition) Known() bool {
	_, ok := known[d]
	return ok
}

// Reason returns a reason carrying only d.
func (d Disposition) Reason() Reason { return Reason{Disposition: d} }

// Because returns a reason for d with a stable code and a human summary.
// Either may be empty; both are normalized when the reason is recorded.
func (d Disposition) Because(code, summary string) Reason {
	return Reason{Disposition: d, Code: code, Summary: summary}
}

// Limits on recorded detail. They bound every persisted and provider-visible
// copy, so one runaway error message can neither bloat the ledger nor flood an
// issue timeline.
const (
	// SummaryLimit bounds the human summary in bytes, ellipsis included.
	SummaryLimit = 240
	// TokenLimit bounds Stage, Code and Actor in bytes.
	TokenLimit = 64
	// MarkerVersion is the version of the provider marker line.
	MarkerVersion = 1
	// MarkerPrefix starts the machine-readable reason line of a provider
	// claim-release comment. It deliberately does not match the release
	// boundary line (goobers-claim-release: run=<id>), which keeps its own
	// historical grammar for claim-epoch detection.
	MarkerPrefix = "goobers-claim-release-reason:"
)

// Reason is the structured cause of one claim release.
type Reason struct {
	// Disposition is the stable class. Empty or unrecognized normalizes to
	// Unknown.
	Disposition Disposition `json:"disposition"`
	// Stage is the workflow stage (or gate) the run ended at, when known.
	Stage string `json:"stage,omitempty"`
	// Code is the stable error or disposition code, when known.
	Code string `json:"code,omitempty"`
	// Summary is a bounded, scrubbed, single-line human explanation.
	Summary string `json:"summary,omitempty"`
	// Actor is the operator identity behind a force-release, when known.
	Actor string `json:"actor,omitempty"`
}

// IsZero reports whether no reason was supplied at all.
func (r Reason) IsZero() bool { return r == Reason{} }

// At returns r with the stage it ended at.
func (r Reason) At(stage string) Reason {
	r.Stage = stage
	return r
}

// By returns r with the acting operator identity.
func (r Reason) By(actor string) Reason {
	r.Actor = actor
	return r
}

// Normalize returns the canonical, bounded and sanitized form of r: an
// unrecognized disposition becomes Unknown, Stage/Code/Actor are reduced to a
// safe token alphabet within TokenLimit, and Summary is secret-scrubbed, has
// local paths and credential assignments redacted, is collapsed to one line
// and is cut to SummaryLimit. Normalize is idempotent, so a reason may be
// normalized at every layer it crosses.
func (r Reason) Normalize() Reason {
	if !r.Disposition.Known() {
		r.Disposition = Unknown
	}
	r.Stage = token(r.Stage)
	r.Code = token(r.Code)
	r.Actor = token(r.Actor)
	r.Summary = Sanitize(r.Summary)
	return r
}

// token keeps identifiers in an alphabet that can never break the marker
// line's key=value grammar, Markdown code spans, or a single-line comment.
func token(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
		case strings.ContainsRune("._-:@+", r):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return truncate.Bytes(b.String(), TokenLimit, "")
}

var (
	scrubber = secretpattern.NewScrubber()
	// credentialAssignment redacts KEY=value / KEY: value pairs whose key
	// names a credential, so an environment dump inside an error never
	// reaches a provider.
	credentialAssignment = regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API_?KEY|PRIVATE_?KEY|CREDENTIAL)[A-Z0-9_]*)(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`)
	// localPath redacts absolute filesystem paths (POSIX, home-relative and
	// Windows drive paths with at least two separators). The preceding byte
	// must be a delimiter other than ':' so URLs (https://host/path) survive.
	localPath = regexp.MustCompile(`(^|[\s"'(\[=,;<>])((?:~|[A-Za-z]:)?[/\\][^\s"'()\[\],;<>]*[/\\][^\s"'()\[\],;<>]*)`)
	spaces    = regexp.MustCompile(`\s+`)
)

// PathPlaceholder replaces a redacted local filesystem path.
const PathPlaceholder = "[path]"

// Sanitize returns text made safe for a provider-visible single line: secret
// shapes scrubbed, credential assignments and local paths redacted, control
// characters and Markdown code fences neutralized, whitespace collapsed, and
// the result bounded to SummaryLimit bytes. Scrubbing happens before the cut
// so truncation can never split a secret into an unrecognizable fragment.
func Sanitize(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	text = string(scrubber.Scrub([]byte(text)))
	text = credentialAssignment.ReplaceAllString(text, "${1}${2}"+secretpattern.Redacted)
	text = localPath.ReplaceAllString(text, "${1}"+PathPlaceholder)
	text = strings.Map(func(r rune) rune {
		switch {
		case r == '`':
			return '\''
		case unicode.IsControl(r):
			return ' '
		}
		return r
	}, text)
	text = strings.TrimSpace(spaces.ReplaceAllString(text, " "))
	return truncate.Bytes(text, SummaryLimit, "…")
}

// Headline is the human description of the release, without its detail.
func (r Reason) Headline() string {
	r = r.Normalize()
	switch r.Disposition {
	case Completed:
		return "run completed"
	case Failed:
		return "run failed"
	case Escalated:
		return "run escalated"
	case Cancelled:
		return "run was cancelled"
	case Abandoned:
		return "run gave up the item before finishing it"
	case NoWork:
		return "run found no work to do"
	case StallSwept:
		return "claim lease expired without renewal (stalled or crashed run) and was swept by recovery"
	case Reconciled:
		return "provider claim marker reconciled with the claim ledger"
	case ForceReleased:
		if r.Actor != "" {
			return "claim force-released by operator `" + r.Actor + "`"
		}
		return "claim force-released by an operator"
	case Superseded:
		return "claim superseded by another owner"
	default:
		return "not recorded (legacy release path)"
	}
}

// Sentence renders the "Reason: ..." line: the headline, the stage, the
// stable code and the bounded summary, each only when known.
func (r Reason) Sentence() string {
	r = r.Normalize()
	var b strings.Builder
	b.WriteString("Reason: ")
	b.WriteString(r.Headline())
	if r.Stage != "" {
		b.WriteString(" at `" + r.Stage + "`")
	}
	switch {
	case r.Code != "" && r.Summary != "":
		b.WriteString(" — " + r.Code + ": " + r.Summary)
	case r.Code != "":
		b.WriteString(" — " + r.Code)
	case r.Summary != "":
		b.WriteString(" — " + r.Summary)
	}
	if !strings.HasSuffix(b.String(), ".") && !strings.HasSuffix(b.String(), "…") {
		b.WriteByte('.')
	}
	return b.String()
}

// Eligibility states what the release means for the item's future.
func (r Reason) Eligibility() string {
	switch r.Normalize().Disposition {
	case Completed:
		return "This run's work on the item is finished; the item's status shows the result."
	case Escalated:
		return "The item may be parked for human attention; it becomes eligible again after remediation."
	case NoWork:
		return "Nothing was changed; the item remains eligible for a later run."
	case Reconciled:
		return "This release only repairs claim-marker drift; no run is working on the item."
	case Superseded:
		return "Another owner holds the item now; this run stopped processing it."
	case Unknown:
		return "A later run may claim this item."
	default:
		return "The item remains eligible and may be claimed again."
	}
}

// MarkerLine renders the versioned machine-readable reason line:
//
//	goobers-claim-release-reason: v=1 disposition=<d> [stage=<s>] [code=<c>] [actor=<a>]
//
// Fields are space-separated key=value pairs in the token alphabet (never the
// prose summary), so a reader splits on whitespace and '=' without escaping. A
// reader must ignore unknown keys and treat a missing line, or another v, as
// an unknown (legacy) release.
func (r Reason) MarkerLine() string {
	r = r.Normalize()
	fields := []string{"v=" + strconv.Itoa(MarkerVersion), "disposition=" + string(r.Disposition)}
	for _, field := range []struct{ key, value string }{{"stage", r.Stage}, {"code", r.Code}, {"actor", r.Actor}} {
		if field.value != "" {
			fields = append(fields, field.key+"="+field.value)
		}
	}
	return MarkerPrefix + " " + strings.Join(fields, " ")
}

// Comment renders the provider-visible claim-release comment for runID. The
// first line is the historical, machine-parsed release boundary
// (goobers-claim-release: run=<id>), unchanged so claim-epoch detection and
// historical comments keep parsing; the reason marker, the human reason and
// the eligibility statement follow.
func Comment(runID string, reason Reason) string {
	return fmt.Sprintf("goobers-claim-release: run=%s\n%s\n\nReleased by Goobers run `%s`.\n%s\n%s",
		runID, reason.MarkerLine(), runID, reason.Sentence(), reason.Eligibility())
}
