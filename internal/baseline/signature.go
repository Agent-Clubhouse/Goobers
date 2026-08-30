package baseline

import (
	"regexp"
	"strings"

	"github.com/goobers/goobers/internal/executor"
)

// executorFailureMessage matches the message a failing shell stage carries:
// the executor renders "command exited <n>; failure: <diagnostic>" (optionally
// followed by "; warnings: ..."), so the diagnostic has to be lifted back out
// of it before it can be compared with a probe's raw transcript.
var executorFailureMessage = regexp.MustCompile(`command exited -?\d+; failure: `)

// failureWarningsSuffix is the trailer applyCommandFailureDiagnostic appends
// when it also recorded a warning window; it names byte offsets in that run's
// artifacts, which are pure noise in a cross-run comparison.
const failureWarningsSuffix = "; warnings: "

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
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	for _, line := range strings.Split(trimmed, "\n") {
		match := executorFailureMessage.FindStringIndex(line)
		if match == nil {
			continue
		}
		diagnostic := line[match[1]:]
		if index := strings.Index(diagnostic, failureWarningsSuffix); index >= 0 {
			diagnostic = diagnostic[:index]
		}
		if diagnostic = strings.TrimSpace(diagnostic); diagnostic != "" {
			return signatureLines(diagnostic)
		}
	}
	if diagnostic := executor.FailureDiagnostic([]byte(trimmed), nil); diagnostic != "" {
		return signatureLines(diagnostic)
	}
	return trimmed
}

// signatureLines splits an extracted failure section back into the lines it was
// joined from and rewrites the "--- FAIL: TestX" header, which
// flake.NormalizeSignature classifies as boilerplate. Dropping it would throw
// away the only part of a one-line diagnostic that names WHICH test failed.
func signatureLines(diagnostic string) string {
	parts := strings.Split(diagnostic, " | ")
	lines := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if rest := strings.TrimPrefix(part, "--- FAIL:"); rest != part {
			part = "failed test: " + strings.TrimSpace(rest)
		}
		lines = append(lines, part)
	}
	return strings.Join(lines, "\n")
}
