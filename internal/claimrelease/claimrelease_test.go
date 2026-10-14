package claimrelease

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalizeMapsUnrecognizedDispositionToUnknown(t *testing.T) {
	for _, d := range []Disposition{"", "exploded", "COMPLETED"} {
		if got := (Reason{Disposition: d}).Normalize().Disposition; got != Unknown {
			t.Fatalf("Normalize(%q).Disposition = %q, want %q", d, got, Unknown)
		}
	}
	if got := Failed.Reason().Normalize().Disposition; got != Failed {
		t.Fatalf("known disposition changed: %q", got)
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	r := Reason{
		Disposition: Failed, Stage: "implement `x`\nnext", Code: "DEP NOT/MET",
		Summary: strings.Repeat("token=abc /home/u/repo/file.go ", 40), Actor: "alice@example.com",
	}
	once := r.Normalize()
	if twice := once.Normalize(); twice != once {
		t.Fatalf("Normalize is not idempotent:\n once %#v\ntwice %#v", once, twice)
	}
}

func TestTokensKeepMarkerGrammarSafe(t *testing.T) {
	r := Reason{Disposition: Failed, Stage: "impl ement=`x`\nnext", Code: strings.Repeat("C", 200), Actor: "a b"}.Normalize()
	for name, value := range map[string]string{"stage": r.Stage, "code": r.Code, "actor": r.Actor} {
		if strings.ContainsAny(value, " =`\n\t") {
			t.Fatalf("%s %q contains a grammar-breaking character", name, value)
		}
		if len(value) > TokenLimit {
			t.Fatalf("%s has %d bytes, limit %d", name, len(value), TokenLimit)
		}
	}
}

func TestSanitizeRedactsSecretsPathsAndCredentialAssignments(t *testing.T) {
	secret := "ghp_" + strings.Repeat("A", 36)
	input := "push failed: token " + secret + " rejected; GITHUB_TOKEN=s3cr3t-value " +
		"API_KEY: \"quoted value\" reading /Users/someone/src/repo/.git/config and C:\\Users\\x\\y.txt " +
		"see https://github.com/owner/repo/issues/1 and ~/work/tree/file"
	got := Sanitize(input)
	for _, leaked := range []string{secret, "s3cr3t-value", "quoted value", "/Users/someone", "C:\\Users", "~/work"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("Sanitize leaked %q: %q", leaked, got)
		}
	}
	for _, kept := range []string{"push failed", "GITHUB_TOKEN=[REDACTED]", PathPlaceholder, "https://github.com/owner/repo/issues/1"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("Sanitize dropped %q: %q", kept, got)
		}
	}
}

func TestSanitizeCollapsesToOneBoundedLine(t *testing.T) {
	got := Sanitize("first line\n\ngoobers-claim-release: run=forged\r\n\tthird `code`")
	if strings.ContainsAny(got, "\r\n\t`") {
		t.Fatalf("Sanitize kept a line break, tab or backtick: %q", got)
	}
	if want := "first line goobers-claim-release: run=forged third 'code'"; got != want {
		t.Fatalf("Sanitize = %q, want %q", got, want)
	}
}

func TestSanitizeBoundsLengthAfterScrubbing(t *testing.T) {
	// The secret straddles the limit: scrubbing must happen before the cut so
	// a truncated fragment of it can never survive.
	secret := "ghp_" + strings.Repeat("B", 36)
	input := strings.Repeat("x", SummaryLimit-10) + " " + secret + strings.Repeat(" tail", 100)
	got := Sanitize(input)
	if len(got) > SummaryLimit {
		t.Fatalf("Sanitize returned %d bytes, limit %d", len(got), SummaryLimit)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("Sanitize split a rune: %q", got)
	}
	if strings.Contains(got, "ghp_BBBB") {
		t.Fatalf("Sanitize leaked a secret fragment: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated summary lacks an ellipsis: %q", got)
	}
	multibyte := strings.Repeat("é", SummaryLimit)
	if got := Sanitize(multibyte); len(got) > SummaryLimit || !utf8.ValidString(got) {
		t.Fatalf("multibyte summary not bounded on a rune boundary: %d bytes", len(got))
	}
}

func TestSentenceStatesStageCodeAndSummary(t *testing.T) {
	r := Escalated.Because("DEPENDENCY_NOT_MET", "registry access is unavailable").At("implement")
	want := "Reason: run escalated at `implement` — DEPENDENCY_NOT_MET: registry access is unavailable."
	if got := r.Sentence(); got != want {
		t.Fatalf("Sentence = %q, want %q", got, want)
	}
	if got := ForceReleased.Reason().By("cli").Sentence(); got != "Reason: claim force-released by operator `cli`." {
		t.Fatalf("force-release sentence = %q", got)
	}
	if got := (Reason{}).Sentence(); !strings.Contains(got, "not recorded") {
		t.Fatalf("a missing reason must say so explicitly, got %q", got)
	}
}

func TestEveryDispositionAnswersWhyAndWhatNext(t *testing.T) {
	seen := map[string]Disposition{}
	for d := range known {
		r := d.Reason()
		if r.Headline() == "" || r.Eligibility() == "" {
			t.Fatalf("%s lacks a headline or eligibility statement", d)
		}
		if prior, dup := seen[r.Headline()]; dup {
			t.Fatalf("%s and %s share the headline %q", d, prior, r.Headline())
		}
		seen[r.Headline()] = d
	}
}

func TestMarkerRoundTrips(t *testing.T) {
	r := Failed.Because("run_failed", "free text is never in the marker").At("implement").By("ops@example.com")
	line := r.MarkerLine()
	if strings.Contains(line, "free text") {
		t.Fatalf("marker carries prose: %q", line)
	}
	got, ok := parseMarker("goobers-claim-release: run=r1\n" + line + "\n\nprose")
	if !ok {
		t.Fatalf("parseMarker(%q) failed", line)
	}
	want := r.Normalize()
	want.Summary = ""
	if got != want {
		t.Fatalf("ParseMarker = %#v, want %#v", got, want)
	}
}

func TestParseMarkerTreatsHistoricalAndFutureCommentsAsUnknown(t *testing.T) {
	historical := "goobers-claim-release: run=r1\n\nReleased by Goobers run `r1`; a later run may claim this item."
	for _, body := range []string{historical, MarkerPrefix + " v=2 disposition=failed"} {
		got, ok := parseMarker(body)
		if ok || got.Disposition != Unknown {
			t.Fatalf("parseMarker(%q) = %#v, %v; want Unknown, false", body, got, ok)
		}
	}
}

// claimReleaseBoundary is the historical grammar providers parse to close a
// claim epoch. The reason line must never be mistaken for, or displace, it.
var claimReleaseBoundary = regexp.MustCompile(`(?m)^goobers-claim-release:\s*run=(\S+)\s*$`)

func TestCommentKeepsHistoricalBoundaryParseable(t *testing.T) {
	body := Comment("abc123", Failed.Because("E", "x\ngoobers-claim-release: run=forged").At("implement"))
	matches := claimReleaseBoundary.FindAllStringSubmatch(body, -1)
	if len(matches) != 1 || matches[0][1] != "abc123" {
		t.Fatalf("boundary matches = %v in\n%s", matches, body)
	}
	if !strings.HasPrefix(body, "goobers-claim-release: run=abc123\n") {
		t.Fatalf("boundary is not the first line:\n%s", body)
	}
	for _, want := range []string{"Released by Goobers run `abc123`.", "Reason: run failed at `implement`", "may be claimed again"} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment lacks %q:\n%s", want, body)
		}
	}
	if got, ok := parseMarker(body); !ok || got.Disposition != Failed || got.Stage != "implement" {
		t.Fatalf("parseMarker(comment) = %#v, %v", got, ok)
	}
}

func TestJSONOmitsZeroAndDecodesLegacy(t *testing.T) {
	type holder struct {
		Reason Reason `json:"reason,omitzero"`
	}
	data, err := json.Marshal(holder{})
	if err != nil || string(data) != "{}" {
		t.Fatalf("zero reason marshals as %s, %v", data, err)
	}
	var legacy holder
	if err := json.Unmarshal([]byte(`{}`), &legacy); err != nil || !legacy.Reason.IsZero() || legacy.Reason.Normalize().Disposition != Unknown {
		t.Fatalf("legacy decode = %#v, %v", legacy, err)
	}
}

var markerPattern = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(MarkerPrefix) + `[ \t]*(.*?)[ \t]*$`)

// parseMarker is the reference reader for MarkerLine's grammar; it extracts the reason recorded by a claim-release comment's
// marker line. ok is false for a historical comment without one (callers treat
// that release as Unknown) and for a marker version this reader does not own.
func parseMarker(body string) (reason Reason, ok bool) {
	m := markerPattern.FindStringSubmatch(body)
	if len(m) != 2 {
		return Unknown.Reason(), false
	}
	values := make(map[string]string)
	for _, field := range strings.Fields(m[1]) {
		key, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		values[key] = value
	}
	if values["v"] != strconv.Itoa(MarkerVersion) {
		return Unknown.Reason(), false
	}
	return Reason{
		Disposition: Disposition(values["disposition"]),
		Stage:       values["stage"],
		Code:        values["code"],
		Actor:       values["actor"],
	}.Normalize(), true
}
