package decisiongate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/secretpattern"
)

// CIFailureThresholdKey is the threshold key for the CI failure triage
// question.
const CIFailureThresholdKey = "ci_failure_class"

// DefaultCIFailureThreshold keeps an ambiguous log on the existing path, where
// every CI failure goes to the implementer.
var DefaultCIFailureThreshold = Threshold{Accept: 0.85, Reject: 0.15}

// CIFailureClass says why a CI run failed. CIUnsure means the caller keeps
// today's behavior and hands the failure to the implementer.
type CIFailureClass string

// CI failure classes.
const (
	CIFlaky       CIFailureClass = "flaky"
	CIReal        CIFailureClass = "real"
	CIInfra       CIFailureClass = "infra"
	CIEnvironment CIFailureClass = "environment"
	CIUnsure      CIFailureClass = "unsure"
)

// Baseline attribution values a caller may pass as a code-decided fact. They
// match internal/baseline's Class values; this package does not import it.
const (
	CIBaselinePRIntroduced = "pr-introduced"
	CIBaselineShared       = "shared-baseline-failure"
)

// CIFailureSource says who decided a class.
type CIFailureSource string

// Sources. Code decides facts first; the model only judges what is left.
const (
	CISourceCode  CIFailureSource = "code"
	CISourceModel CIFailureSource = "model"
	CISourceNone  CIFailureSource = "none"
)

const (
	// MaxCILogBytes bounds the redacted excerpt sent to the model.
	MaxCILogBytes = 8 << 10
	// maxCILogHead is the share of the excerpt kept from the start of the log;
	// the rest comes from the end, where CI tools print their verdict.
	maxCILogHead   = 1 << 10
	ciTruncMarker  = "\n[... log truncated ...]\n"
	ciPlaceholder  = "[REDACTED]"
	ciHomeReplaced = "<home>"
)

var (
	ciANSI       = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")
	ciURLUser    = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@:]+(?::[^\s/@]*)?@`)
	ciAuthHeader = regexp.MustCompile(`(?i)\b(authorization|proxy-authorization)\b\s*[:=]\s*(?:bearer|basic|token)?\s*\S+`)
	ciBearer     = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{12,}`)
	ciAssign     = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:token|secret|password|passwd|pwd|api[_-]?key|access[_-]?key|credential)[a-z0-9_.-]*)(["']?\s*[:=]\s*["']?)[^\s"',;]+`)
	ciEmail      = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	ciHome       = regexp.MustCompile(`(?i)(?:[A-Za-z]:\\Users\\|/Users/|/home/)[^\\/\s"']+`)

	ciScrubber = secretpattern.NewScrubber()

	// ciInfraSignals are known infrastructure faults. A hit is a code-decided
	// fact: the failure is not in the change under test.
	ciInfraSignals = []*regexp.Regexp{
		regexp.MustCompile(`(?i)the runner has received a shutdown signal`),
		regexp.MustCompile(`(?i)runner (?:lost communication|is no longer responding)`),
		regexp.MustCompile(`(?i)no space left on device`),
		regexp.MustCompile(`(?i)\b(?:econnreset|etimedout|econnrefused)\b`),
		regexp.MustCompile(`(?i)tls handshake timeout`),
		regexp.MustCompile(`(?i)could not resolve host`),
		regexp.MustCompile(`(?i)\b50[234] (?:bad gateway|service unavailable|gateway time-?out)\b`),
		regexp.MustCompile(`(?i)(?:api )?rate limit exceeded`),
		regexp.MustCompile(`(?i)failed to (?:download|fetch) (?:action|artifact)`),
	}
	// ciEnvironmentSignals are missing-tool faults in the build environment.
	ciEnvironmentSignals = []*regexp.Regexp{
		regexp.MustCompile(`(?i)executable file not found in \$?path`),
		regexp.MustCompile(`(?i)\bcommand not found\b`),
		regexp.MustCompile(`(?i)is not recognized as an internal or external command`),
	}
)

var ciFailureQuestion = decider.Choice(
	"Given only this redacted excerpt of a failed CI log, which one best explains the failure?",
	map[string]any{
		string(CIFlaky):       "The test or step passes on retry; the failure is nondeterministic (timing, ordering, a race) and not caused by the change.",
		string(CIReal):        "A deterministic defect in the change under test: a compile error, a failing assertion, or a lint or vet finding.",
		string(CIInfra):       "The CI service or network failed: runner loss, timeouts, outages, rate limits.",
		string(CIEnvironment): "The build environment is wrong: a missing tool, wrong version, missing permission or misconfiguration.",
	},
)

// CILogExcerpt is the only form of a CI log that may leave the process: scrubbed
// of credentials, then bounded.
type CILogExcerpt struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
	// OriginalBytes is the size of the raw log, so shadow scores can be
	// compared across log sizes.
	OriginalBytes int `json:"originalBytes"`
}

// RedactCILog removes ANSI escapes, credentials and personal data from raw CI
// output. It is deliberately aggressive: a false positive costs a little
// context, a miss leaks a secret to a model.
func RedactCILog(raw string) string {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = ciANSI.ReplaceAllString(s, "")
	s = string(ciScrubber.Scrub([]byte(s)))
	s = ciURLUser.ReplaceAllString(s, "${1}"+ciPlaceholder+"@")
	s = ciAuthHeader.ReplaceAllString(s, "${1}: "+ciPlaceholder)
	s = ciBearer.ReplaceAllString(s, "${1} "+ciPlaceholder)
	s = ciAssign.ReplaceAllString(s, "${1}${2}"+ciPlaceholder)
	s = ciEmail.ReplaceAllString(s, ciPlaceholder)
	return ciHome.ReplaceAllStringFunc(s, func(m string) string {
		i := strings.LastIndexAny(m, `\/`)
		return m[:i+1] + ciHomeReplaced
	})
}

// prepareCILog redacts raw and then bounds it to MaxCILogBytes, keeping the
// start and the end of the log. Redaction runs first so a secret cannot be
// split by the cut and slip past a pattern. It also returns the whole redacted
// log for fact checks.
func prepareCILog(raw string) (string, CILogExcerpt) {
	s := RedactCILog(raw)
	ex := CILogExcerpt{OriginalBytes: len(raw)}
	if len(s) <= MaxCILogBytes {
		ex.Text = s
		return s, ex
	}
	tail := MaxCILogBytes - maxCILogHead - len(ciTruncMarker)
	head := cutHead(s, maxCILogHead)
	end := cutTail(s, tail)
	ex.Text, ex.Truncated = head+ciTruncMarker+end, true
	return s, ex
}

func cutHead(s string, n int) string {
	s = s[:n]
	if i := strings.LastIndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func cutTail(s string, n int) string {
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[1:]
	}
	return s
}

// CIFailureFacts decides a class in code, without a model. A shared baseline
// failure is environment (the target branch already fails the same way); a
// known infrastructure or missing-tool signature in the log is infra or
// environment. ok is false when code cannot decide. It expects redacted text.
func CIFailureFacts(redacted, baseline string) (CIFailureClass, string, bool) {
	if baseline == CIBaselineShared {
		return CIEnvironment, "the target branch fails identically at the pinned base", true
	}
	for _, re := range ciInfraSignals {
		if re.MatchString(redacted) {
			return CIInfra, "known infrastructure signature: " + re.String(), true
		}
	}
	for _, re := range ciEnvironmentSignals {
		if re.MatchString(redacted) {
			return CIEnvironment, "known missing-tool signature: " + re.String(), true
		}
	}
	return CIUnsure, "", false
}

// CIFailureResult is an advisory classification. It carries a digest of what
// was sent, never the log.
type CIFailureResult struct {
	Class       CIFailureClass  `json:"class"`
	Source      CIFailureSource `json:"source"`
	Reason      string          `json:"reason,omitempty"`
	Probability float64         `json:"probability,omitempty"`
	Digest      string          `json:"digest"`
	Truncated   bool            `json:"truncated,omitempty"`
	Cached      bool            `json:"cached,omitempty"`
}

type ciFailureState struct {
	Log      string `json:"log"`
	Baseline string `json:"baseline,omitempty"`
}

// EvaluateCIFailure classifies a failed CI log as flaky, real, infra or
// environment. Code facts run first and never reach the model. Otherwise the
// model picks one class from a redacted, size-bounded excerpt, accepted only
// above the ci_failure_class threshold. Uncertain answers and errors return
// CIUnsure so the caller's existing path (the implementer) runs.
func (g *Gate) EvaluateCIFailure(ctx context.Context, rawLog, baseline string) (CIFailureResult, Outcome, error) {
	full, ex := prepareCILog(rawLog)
	sum := sha256.Sum256([]byte(ex.Text + "\x00" + baseline))
	res := CIFailureResult{Class: CIUnsure, Source: CISourceNone, Digest: "sha256:" + hex.EncodeToString(sum[:]), Truncated: ex.Truncated}
	none := Outcome{Name: CIFailureThresholdKey}
	if strings.TrimSpace(ex.Text) == "" {
		return res, none, nil
	}
	// Facts scan the whole redacted log so a signature in the cut middle is
	// not missed.
	if class, reason, ok := CIFailureFacts(full, baseline); ok {
		res.Class, res.Source, res.Reason = class, CISourceCode, reason
		return res, none, nil
	}
	o, err := g.JudgeChoice(ctx, CIFailureThresholdKey, ciFailureState{Log: ex.Text, Baseline: baseline}, ciFailureQuestion)
	res.Probability, res.Cached = o.Probability, o.Cached
	if err != nil {
		return res, o, err
	}
	if o.Decision == Yes {
		switch class := CIFailureClass(o.Choice); class {
		case CIFlaky, CIReal, CIInfra, CIEnvironment:
			res.Class, res.Source = class, CISourceModel
		}
	}
	return res, o, nil
}
