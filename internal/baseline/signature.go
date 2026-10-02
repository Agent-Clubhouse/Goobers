package baseline

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/flake"
)

// executorFailureMessage matches the message a failing shell stage carries:
// the executor renders "command exited <n>; failure: <diagnostic>" (optionally
// followed by run-local trailers), so the diagnostic has to be lifted back out
// of it before it can be compared with a probe's raw transcript.
var executorFailureMessage = regexp.MustCompile(`command exited -?\d+; failure: `)

// failureMessageTrailer matches the trailers applyCommandFailureDiagnostic
// appends after the diagnostic: a dependency-denial hint, the warning window's
// byte offsets, and the failureDigest line count (#5101). Each describes that
// run's own artifacts rather than the failure, and a probe's raw transcript
// never carries any of them, so leaving one in makes a shared failure look
// branch-introduced (#4477).
var failureMessageTrailer = regexp.MustCompile(`; (?:hint|warnings): |; \d+ distinct failure line\(s\) recorded in `)

// platformQualifier matches a finding's `[platforms: a,b]` suffix: the form a
// gate that evaluates several target platforms from one host (test/deadcode,
// #4434) uses to say a finding holds on only some of them.
var platformQualifier = regexp.MustCompile(`\[platforms: ([^\]]+)\]`)

// failureSignature is the comparable identity of one piece of failure
// evidence: flake.NormalizeSignature over its reduced diagnostic — the
// readable part — extended with a digest of its whole roster
// (flake.RosterLines: every line, in order, file:line kept). The readable part
// alone keeps three de-duplicated lines with their locations stripped, so a
// branch adding a fourth finding, or repeating an inherited lint message in a
// new file, would share the base's signature and be parked behind a failure
// that is partly its own (#4477). The roster errs toward difference — a moved
// line number is a different failure — which only ever fails open. Paths are
// reduced to their basename, so the same message at the same line of two
// same-named files in different directories still reads alike; findings that
// name their subject in full (a deadcode symbol) are unaffected.
//
// Evidence whose diagnostic the executor cut at its size bound is marked with
// truncatedSignatureSuffix: whatever followed the cut is unseen, so two such
// signatures being equal proves nothing, and Classify refuses to call them
// identical.
func failureSignature(text string) string {
	diagnostic, extracted := failureDiagnostic(text)
	return reducedSignature(FailureSignatureText(text), extracted && executor.DiagnosticTruncated(diagnostic))
}

// probeSignature is failureSignature for a probe: the diagnostic is extracted
// from the probe's separate streams exactly as the shell executor extracts the
// run's own, so both halves see the same window (a combined transcript would
// lead with stdout chatter the run's stderr window never had). A prober that
// could not separate the streams falls back to the raw transcript.
func probeSignature(result ProbeResult) string {
	if diagnostic := executor.FailureDiagnostic([]byte(result.Output), []byte(result.Stderr)); diagnostic != "" {
		return reducedSignature(signatureLines(diagnostic), executor.DiagnosticTruncated(diagnostic))
	}
	return failureSignature(strings.TrimSpace(result.Output + "\n" + result.Stderr))
}

func reducedSignature(reduced string, truncated bool) string {
	signature := flake.NormalizeSignature(reduced)
	if roster := flake.RosterLines(reduced); len(roster) > 0 {
		sum := sha256.Sum256([]byte(strings.Join(roster, "\n")))
		signature = fmt.Sprintf("%s [roster: %d line(s), sha256:%x]", signature, len(roster), sum[:8])
	}
	if truncated {
		signature += truncatedSignatureSuffix
	}
	return signature
}

// failureRoster identifies a failure digest by the SET of its failure lines
// (executor.IsFailureLine), each normalized across checkouts
// (flake.NormalizeVolatile: run-specific values replaced, file:line kept). The
// digest also carries context windows; their non-finding lines (module
// downloads, tool chatter) vary between a warm and a cold checkout and are not
// part of the failure. complete is false when the digest was cut at its size
// bound, because then part of the roster is unseen.
func failureRoster(digest string) (string, bool) {
	if executor.DigestTruncated(digest) {
		return "", false
	}
	var lines []string
	for _, line := range strings.Split(digest, "\n") {
		if executor.IsFailureLine(line) {
			lines = append(lines, flake.NormalizeVolatile(line))
		}
	}
	slices.Sort(lines)
	lines = slices.Compact(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return fmt.Sprintf("%d:%x", len(lines), sum[:8]), true
}

// truncatedSignatureSuffix marks a signature derived from a truncated
// diagnostic (see failureSignature).
const truncatedSignatureSuffix = " [truncated]"

// signatureTruncated reports whether failureSignature marked signature as
// derived from truncated evidence.
func signatureTruncated(signature string) bool {
	return strings.HasSuffix(signature, truncatedSignatureSuffix)
}

// FailureSignatureText reduces one piece of failure evidence to the diagnostic
// a signature is derived from. Both halves of a baseline comparison go through
// it, because they start from structurally different text: the run side holds a
// stage result whose summary/message is the executor's ALREADY-extracted
// diagnostic ("command exited 2; failure: --- FAIL: TestX | expected 1 got 2"),
// while the probe side holds the raw combined output of the same command. Left
// unreduced the two never produce the same signature, so a genuine shared
// baseline failure would be blamed on every branch that hit it — the exact
// churn #2971 exists to stop.
//
// The result is newline-separated evidence lines ready for
// flake.NormalizeSignature, with the go test header rewritten (see
// signatureLines): the extractor joins a failure section into one line, and a
// single line beginning "--- FAIL:" is boilerplate to the normalizer, which
// would reduce EVERY test failure to the same placeholder signature and make
// unrelated failures look shared.
//
// Text that carries no recognizable failure diagnostic is returned trimmed but
// otherwise unchanged: reducing it further would invent a match, and an
// unmatched fingerprint fails open to the pre-existing attribution.
func FailureSignatureText(text string) string {
	if diagnostic, ok := failureDiagnostic(text); ok {
		return signatureLines(diagnostic)
	}
	return strings.TrimSpace(text)
}

// failureDiagnostic lifts the executor's failure diagnostic out of text: from
// a stage message when text carries one, else by extracting it from raw
// output. ok is false when text carries no recognizable diagnostic.
func failureDiagnostic(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", false
	}
	if match := executorFailureMessage.FindStringIndex(trimmed); match != nil {
		diagnostic := trimmed[match[1]:]
		// A stage result repeats its message as the summary, so the text can
		// hold the same message twice; the diagnostic ends where the next copy
		// begins. It may itself span several lines (a window of several
		// findings), so it is NOT cut at the first newline (#4477).
		if next := executorFailureMessage.FindStringIndex(diagnostic); next != nil {
			diagnostic = diagnostic[:next[0]]
		}
		diagnostic = strings.TrimSpace(diagnostic)
		// The trailers follow the diagnostic, so they can only be on its last
		// line; an earlier finding that happens to contain "; hint: " is
		// evidence, not a trailer.
		lastLine := strings.LastIndexByte(diagnostic, '\n') + 1
		if trailer := failureMessageTrailer.FindStringIndex(diagnostic[lastLine:]); trailer != nil {
			diagnostic = diagnostic[:lastLine+trailer[0]]
		}
		if diagnostic = strings.TrimSpace(diagnostic); diagnostic != "" {
			return diagnostic, true
		}
	}
	if diagnostic := executor.FailureDiagnostic([]byte(trimmed), nil); diagnostic != "" {
		return diagnostic, true
	}
	return "", false
}

// signatureLines splits an extracted failure section back into the lines it was
// joined from, drops the context preceding its first actual finding, and
// rewrites the "--- FAIL: TestX" header, which flake.NormalizeSignature
// classifies as boilerplate. Dropping the header would throw away the only part
// of a one-line diagnostic that names WHICH test failed.
//
// Leading context is dropped because the two halves see it differently: a
// stage's window is cut from its separated stderr, while the probe's comes from
// one combined transcript that also holds the recipe make echoed to stdout
// ("go run ./test/deadcode ..."). A section with no recognizable finding keeps
// every line.
func signatureLines(diagnostic string) string {
	var lines []string
	for _, line := range strings.Split(diagnostic, "\n") {
		for _, part := range strings.Split(line, " | ") {
			if part = strings.TrimSpace(part); part != "" {
				lines = append(lines, part)
			}
		}
	}
	if first := slices.IndexFunc(lines, executor.IsFailureLine); first > 0 {
		lines = lines[first:]
	}
	for index, part := range lines {
		if rest := strings.TrimPrefix(part, "--- FAIL:"); rest != part {
			lines[index] = "failed test: " + strings.TrimSpace(rest)
		}
	}
	return strings.Join(lines, "\n")
}

// FailurePlatforms returns the sorted, de-duplicated target platforms a
// failure's findings are qualified to (`[platforms: windows]`), or nil when no
// finding is platform-qualified. A shared baseline failure carrying them is one
// the base reproduces only for those platforms — the context a remediation
// reader needs to tell an inherited platform-specific failure from an ordinary
// shared one (#4477).
func FailurePlatforms(text string) []string {
	var platforms []string
	for _, match := range platformQualifier.FindAllStringSubmatch(text, -1) {
		for _, platform := range strings.Split(match[1], ",") {
			if platform = strings.TrimSpace(platform); platform != "" && !slices.Contains(platforms, platform) {
				platforms = append(platforms, platform)
			}
		}
	}
	slices.Sort(platforms)
	return platforms
}
