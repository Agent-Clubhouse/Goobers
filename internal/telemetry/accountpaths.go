package telemetry

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
	"unicode"
)

// accountPathPlaceholder replaces a home-directory root in exported text when
// the destination has no host/account identity consent (#6770). It must stay
// equal to instance.TelemetryCollectionContract().ConsentGatedPathPlaceholder.
const accountPathPlaceholder = "<home>"

// accountRootField is dropped from exported journal documents without identity
// consent: daemon lifecycle events record the absolute instance root, which
// usually sits beneath the daemon account's home directory. The durable
// goobers.instance.id resource attribute already correlates those records.
const accountRootField = "instanceRoot"

// A path segment that is an account name ends at a separator, whitespace,
// quoting, or punctuation that cannot be part of a POSIX/Windows user name in
// command-line or error text.
const accountSegment = `[^\\/\s"'` + "`" + `<>|:;,()\[\]{}*?]+`

var (
	// macOS and Linux home roots. Case-sensitive so URL paths such as
	// /api/users/42 are not mistaken for an account directory.
	posixAccountRoot = regexp.MustCompile(`/(?:Users|home)/` + accountSegment)
	// Windows profile roots, with a drive (C:\Users\x, c:/users/x, including
	// \\?\C:\ and JSON-escaped C:\\Users\\x) or as a UNC/driveless \Users\x.
	// A profile folder name may contain spaces ("C:\Users\Jane Doe\..."), so a
	// segment followed by a backslash may include spaces; otherwise it stops at
	// whitespace like the POSIX form.
	windowsAccountRoot = regexp.MustCompile(`(?i)(?:\b[a-z]:[\\/]+|\\)users[\\/]+(?:[^\\/"'<>|:*?;\r\n\t]{1,128}\\|` + accountSegment + `)`)
)

// accountPathScrubber withholds account-identifying filesystem paths from text
// that leaves the instance. It is the single implementation used for every
// exported Azure Monitor signal (journal Logs, diagnostics, and trace spans).
type accountPathScrubber struct {
	// home matches the daemon account's own home directory, which may live
	// outside the conventional roots (for example /srv/<account>).
	home *regexp.Regexp
}

// processAccountPaths resolves the daemon account's home directory once.
var processAccountPaths = sync.OnceValue(func() accountPathScrubber {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return newAccountPathScrubber(home)
})

func newAccountPathScrubber(home string) accountPathScrubber {
	home = strings.TrimRight(home, `/\`)
	if len(home) < 2 {
		// An empty or root ("/") home names no account and would match every path.
		return accountPathScrubber{}
	}
	var pattern strings.Builder
	pattern.WriteString(`(?i)`)
	for _, r := range home {
		if r == '/' || r == '\\' {
			pattern.WriteString(`[\\/]+`)
			continue
		}
		pattern.WriteString(regexp.QuoteMeta(string(r)))
	}
	if last := rune(home[len(home)-1]); unicode.IsLetter(last) || unicode.IsDigit(last) || last == '_' {
		// /srv/alice must not rewrite /srv/alice2 into "<home>2".
		pattern.WriteString(`\b`)
	}
	return accountPathScrubber{home: regexp.MustCompile(pattern.String())}
}

// text replaces every home-directory root in s with accountPathPlaceholder,
// preserving the rest of the path: "/Users/<account>/src/x" becomes
// "<home>/src/x" and `C:\Users\<account>\x` becomes `<home>\x`.
func (a accountPathScrubber) text(s string) string {
	if a.home != nil {
		s = a.home.ReplaceAllLiteralString(s, accountPathPlaceholder)
	}
	s = windowsAccountRoot.ReplaceAllStringFunc(s, func(match string) string {
		if strings.HasSuffix(match, `\`) {
			// Keep the separator consumed by the space-tolerant segment form.
			return accountPathPlaceholder + `\`
		}
		return accountPathPlaceholder
	})
	return posixAccountRoot.ReplaceAllLiteralString(s, accountPathPlaceholder)
}

// candidate is a cheap prefilter on an encoded line. A false positive only
// costs a decode; a line without any match is never rewritten. The patterns
// tolerate JSON-escaped backslashes, so they hold on encoded envelopes.
func (a accountPathScrubber) candidate(line []byte) bool {
	return bytes.Contains(bytes.ToLower(line), []byte(strings.ToLower(accountRootField))) ||
		posixAccountRoot.Match(line) || windowsAccountRoot.Match(line) ||
		(a.home != nil && a.home.Match(line))
}

// value withholds account paths throughout a decoded JSON value. String values
// holding a nested JSON document (a committed journal body is exported as the
// message string) are projected structurally so escaping cannot hide a path.
func (a accountPathScrubber) value(v any) (any, bool) {
	switch typed := v.(type) {
	case string:
		return a.document(typed)
	case map[string]any:
		changed := false
		for key, item := range typed {
			if strings.EqualFold(key, accountRootField) {
				delete(typed, key)
				changed = true
				continue
			}
			if projected, ok := a.value(item); ok {
				typed[key] = projected
				changed = true
			}
		}
		return typed, changed
	case []any:
		changed := false
		for i, item := range typed {
			if projected, ok := a.value(item); ok {
				typed[i] = projected
				changed = true
			}
		}
		return typed, changed
	default:
		return v, false
	}
}

func (a accountPathScrubber) document(s string) (string, bool) {
	if trimmed := strings.TrimSpace(s); (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && json.Valid([]byte(trimmed)) {
		if nested, err := decodeJSONNumbers([]byte(trimmed)); err == nil {
			projected, changed := a.value(nested)
			if !changed {
				return s, false
			}
			if encoded, err := encodeJSONText(projected); err == nil {
				return encoded, true
			}
		}
	}
	// Plain text, or not a complete document (for example a message truncated
	// at the ingestion limit): scrub it as text.
	scrubbed := a.text(s)
	return scrubbed, scrubbed != s
}

// decodeJSONNumbers decodes one JSON value that the caller has validated,
// keeping numbers verbatim so re-encoding never loses integer precision.
func decodeJSONNumbers(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	err := decoder.Decode(&value)
	return value, err
}

// encodeJSONText re-encodes a nested document without HTML escaping so the
// placeholder stays queryable as the literal "<home>" in Log Analytics.
func encodeJSONText(value any) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buffer.String(), "\n"), nil
}
