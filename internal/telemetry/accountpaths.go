package telemetry

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
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

const (
	// pathStart is what may precede an absolute path in command-line, error or
	// JSON text: the start of the string, whitespace, quoting, an assignment or
	// list delimiter, optionally followed by a file:// scheme. A root preceded by
	// a path or URL character (web/src/pages/home/x, /v1.0/Users/abc) is a
	// relative or URL segment, not an account directory.
	pathStart = `((?:^|[\s"'` + "`" + `=(\[{,;:<>|])(?:file://)?)`
	// accountSegment is one account-name path segment. It ends at a separator,
	// whitespace, quoting, or punctuation that cannot be part of a user name.
	accountSegment = `[^\\/\s"'` + "`" + `<>|:;,()\[\]{}*?]+`
	// Windows roots: a drive (C:\Users, c:/users, also behind \\?\ or \\.\) or
	// a UNC share (\\host\c$\Users). Backslash runs tolerate JSON escaping.
	windowsDriveRoot = `(?:\\+[?.]\\+)?[a-z]:[\\/]+users[\\/]+`
	windowsUNCRoot   = `\\{2,}[^\\/\s"'?*<>|:]+\\+[^\\/\s"'?*<>|:]+\\+users\\+`
	windowsRoot      = `(?:` + windowsDriveRoot + `|` + windowsUNCRoot + `)`
)

var (
	// macOS /Users/<a>, Linux /home/<a> and /var/home/<a> (image-based
	// distributions), and WSL /mnt/<drive>/Users/<a>. Case-sensitive, as on
	// the platforms that use them. Group 1 is the preserved path start.
	posixAccountRoot = regexp.MustCompile(pathStart + `((?:/var)?/home/` + accountSegment +
		`|/Users/` + accountSegment + `|/mnt/[A-Za-z]/[Uu]sers/` + accountSegment + `)`)
	// Windows profile roots. A profile folder name may contain spaces
	// ("C:\Users\Jane Doe\AppData"): up to four space-separated words are one
	// segment only when a backslash immediately follows the last word (group
	// 3, preserved); prose such as `C:\Users\alice has no access to \\srv`
	// falls back to the single-word form (group 4).
	windowsAccountRoot = regexp.MustCompile(`(?i)` + pathStart + `(?:(` + windowsRoot + accountSegment +
		`(?: ` + accountSegment + `){1,3})(\\+)|(` + windowsRoot + accountSegment + `))`)
)

// accountPathScrubber withholds account-identifying filesystem paths from text
// that leaves the instance. It is the single implementation used for every
// exported Azure Monitor signal (journal Logs, diagnostics, and trace spans).
type accountPathScrubber struct {
	// home matches the daemon account's own home directory at a path start,
	// which may live outside the conventional roots (for example
	// /srv/<account>). Group 1 is the preserved path start.
	home *regexp.Regexp
	// homeMarker is the lowercase final home path element for the prefilter.
	homeMarker []byte
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
	// Windows paths are case-insensitive; POSIX paths are not (/Root is not
	// /root).
	if strings.Contains(home, `\`) || len(home) > 1 && home[1] == ':' {
		pattern.WriteString(`(?i)`)
	}
	pattern.WriteString(pathStart + `(`)
	for _, r := range home {
		if r == '/' || r == '\\' {
			pattern.WriteString(`[\\/]+`)
			continue
		}
		pattern.WriteString(regexp.QuoteMeta(string(r)))
	}
	pattern.WriteString(`)`)
	last := home[strings.LastIndexAny(home, `/\`)+1:]
	return accountPathScrubber{
		home:       regexp.MustCompile(pattern.String()),
		homeMarker: bytes.ToLower([]byte(last)),
	}
}

// text replaces every home-directory root in s with accountPathPlaceholder,
// preserving the rest of the path: "/Users/<account>/src/x" becomes
// "<home>/src/x" and `C:\Users\<account>\x` becomes `<home>\x`.
func (a accountPathScrubber) text(s string) string {
	if a.home != nil {
		s = replaceAccountRoots(a.home, s, func(match []int) (string, bool) {
			// A whole-name boundary, checked by rune: home /srv/josé must not
			// rewrite /srv/joséphine. Go regexp has no lookahead and \b is ASCII.
			return accountPathPlaceholder, !continuesPathName(s[match[1]:])
		})
	}
	s = replaceAccountRoots(windowsAccountRoot, s, func(match []int) (string, bool) {
		if match[6] >= 0 {
			return accountPathPlaceholder + s[match[6]:match[7]], true
		}
		return accountPathPlaceholder, true
	})
	return replaceAccountRoots(posixAccountRoot, s, func([]int) (string, bool) {
		return accountPathPlaceholder, true
	})
}

// replaceAccountRoots rewrites each match of pattern, whose group 1 is a
// preserved path start, with the replacement returned for it.
func replaceAccountRoots(pattern *regexp.Regexp, s string, replace func(match []int) (string, bool)) string {
	matches := pattern.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	var out strings.Builder
	last := 0
	for _, match := range matches {
		replacement, ok := replace(match)
		if !ok {
			continue
		}
		out.WriteString(s[last:match[3]]) // Text before the match and its path start.
		out.WriteString(replacement)
		last = match[1]
	}
	out.WriteString(s[last:])
	return out.String()
}

// continuesPathName reports whether rest (the text after a home-directory
// match) continues the same file name, making the match a prefix of a
// different directory (/srv/alice2, /srv/alice.bak). A trailing sentence
// period ("in /srv/alice.") ends the name.
func continuesPathName(rest string) bool {
	next, size := utf8.DecodeRuneInString(rest)
	if next == '.' {
		next, _ = utf8.DecodeRuneInString(rest[size:])
	}
	return rest != "" && (unicode.IsLetter(next) || unicode.IsDigit(next) || unicode.IsMark(next) || strings.ContainsRune("_-.~@$+", next))
}

// candidateMarkers make the prefilter catch every root and the dropped field,
// including JSON-escaped spellings (\/Users\/, \u002f) that only the
// structural decode reveals, like the service-health prefilter's \u check.
var candidateMarkers = [][]byte{
	[]byte("users"), []byte("/home/"), []byte(strings.ToLower(accountRootField)), []byte(`\/`), []byte(`\u`),
}

// candidate is a cheap prefilter on an encoded line. A false positive only
// costs a decode; a line without any match is never rewritten.
func (a accountPathScrubber) candidate(line []byte) bool {
	lower := bytes.ToLower(line)
	for _, marker := range candidateMarkers {
		if bytes.Contains(lower, marker) {
			return true
		}
	}
	return len(a.homeMarker) > 0 && bytes.Contains(lower, a.homeMarker)
}

// value withholds account paths throughout a decoded JSON value: object keys
// and string values. String values holding a nested JSON document (a
// committed journal body is exported as the message string) are projected
// structurally so escaping cannot hide a path.
func (a accountPathScrubber) value(v any) (any, bool) {
	switch typed := v.(type) {
	case string:
		return a.document(typed)
	case map[string]any:
		return a.object(typed)
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

func (a accountPathScrubber) object(fields map[string]any) (map[string]any, bool) {
	changed := false
	renamed := make(map[string]any)
	for key, item := range fields {
		if strings.EqualFold(key, accountRootField) {
			delete(fields, key)
			changed = true
			continue
		}
		projected, valueChanged := a.value(item)
		if scrubbed := a.text(key); scrubbed != key {
			// Apply renames after the walk so a new key is never revisited.
			delete(fields, key)
			renamed[scrubbed] = projected
			changed = true
			continue
		}
		if valueChanged {
			fields[key] = projected
			changed = true
		}
	}
	for key, item := range renamed {
		fields[key] = item
	}
	return fields, changed
}

func (a accountPathScrubber) document(s string) (string, bool) {
	if trimmed := strings.TrimSpace(s); (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && json.Valid([]byte(trimmed)) {
		if nested, err := decodeJSONNumbers([]byte(trimmed)); err == nil {
			if projected, changed := a.value(nested); changed {
				if encoded, err := encodeJSONText(projected); err == nil {
					return encoded, true
				}
			}
		}
	}
	// Plain text, a document the walk left unchanged, or an incomplete document
	// (for example a message truncated at the ingestion limit).
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
