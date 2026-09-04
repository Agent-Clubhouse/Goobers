package flake

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

const signatureLimit = 1024

var (
	leadingSourceLocation = regexp.MustCompile(`^.*?\.go:\d+:\s*`)
	sourceLocation        = regexp.MustCompile(`(?:[A-Za-z]:)?(?:[^\s:()]+[\\/])*([^\s:()\\/]+\.go):(\d+)`)
	volatileAddress       = regexp.MustCompile(`0x[0-9a-fA-F]+`)
	volatileDuration      = regexp.MustCompile(`\b(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+(?:\b|$)`)
	volatileGoroutine     = regexp.MustCompile(`\bgoroutine\s+\d+\b`)
	volatileTimestamp     = regexp.MustCompile(`\b20\d\d-\d\d-\d\d[T ][0-9:.+-]+Z?\b`)
	volatileUUID          = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	// volatileTempSuffix is the random tail os.MkdirTemp (and so t.TempDir)
	// appends to the directory name it is given: /tmp/TestFoo833401532 and
	// /tmp/TestFoo1845522470 are the same directory from two runs.
	volatileTempSuffix = regexp.MustCompile(`([\\/][^\s:()\\/]*?)\d{6,}\b`)
	// volatileHexSegment is a whole path segment of hex — a content hash the
	// system under test derived (a mirror's repo key, an object directory),
	// stable within a run and different in the next.
	volatileHexSegment = regexp.MustCompile(`([\\/])[0-9a-fA-F]{8,}\b`)
	// volatileTestFlagValue is the Go test runner echoing its own flags on a
	// failing package (-test.shuffle 1788254672532515140). A numeric value
	// there is a seed or a limit the harness chose for this run, not anything
	// that distinguishes one failure from another.
	volatileTestFlagValue = regexp.MustCompile(`(-test\.[A-Za-z0-9_.]+)([= ])\d[^\s]*`)
)

// NormalizeSignature removes volatile values while retaining the assertion,
// panic, or race sites that distinguish separate failures.
func NormalizeSignature(text string) string {
	lines := strings.Split(text, "\n")
	if signature, ok := normalizeRaceSignature(lines); ok {
		return boundSignature(signature)
	}
	for index, line := range lines {
		if !strings.Contains(strings.TrimSpace(line), "panic:") {
			continue
		}
		signature := []string{normalizeLine(line)}
		for _, stackLine := range lines[index+1:] {
			if strings.Contains(stackLine, "/runtime/") || strings.Contains(stackLine, "/testing/") {
				continue
			}
			match := sourceLocation.FindStringSubmatch(stackLine)
			if len(match) == 3 {
				signature = append(signature, match[1]+":"+match[2])
				break
			}
		}
		return boundSignature(strings.Join(signature, " | "))
	}

	signature := make([]string, 0, 3)
	seen := make(map[string]bool)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if failureBoilerplate(line) {
			continue
		}
		line = normalizeLine(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		signature = append(signature, line)
		if len(signature) == 3 {
			break
		}
	}
	if len(signature) == 0 {
		return "test failed without stable signature"
	}
	return boundSignature(strings.Join(signature, " | "))
}

// Fingerprint returns the ledger identity for a package, test, and normalized
// failure signature.
func Fingerprint(pkg, test, signature string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(pkg+"\x00"+test+"\x00"+signature)))
}

func boundSignature(signature string) string {
	runes := []rune(signature)
	if len(runes) <= signatureLimit {
		return signature
	}
	suffix := fmt.Sprintf("… [sha256:%x]", sha256.Sum256([]byte(signature)))
	suffixRunes := []rune(suffix)
	return string(runes[:signatureLimit-len(suffixRunes)]) + suffix
}

func normalizeRaceSignature(lines []string) (string, bool) {
	warning := -1
	for index, line := range lines {
		if strings.TrimSpace(line) == "WARNING: DATA RACE" {
			warning = index
			break
		}
	}
	if warning == -1 {
		return "", false
	}
	signature := []string{"WARNING: DATA RACE"}
	function := ""
	siteLocated := false
	for _, line := range lines[warning+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.Trim(trimmed, "=") == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "Read at ") ||
			strings.HasPrefix(trimmed, "Write at ") ||
			strings.HasPrefix(trimmed, "Previous read at ") ||
			strings.HasPrefix(trimmed, "Previous write at ") {
			signature = append(signature, normalizeLine(trimmed))
			function = ""
			siteLocated = false
			continue
		}
		if siteLocated {
			continue
		}
		match := sourceLocation.FindStringSubmatch(trimmed)
		if len(match) == 3 {
			if strings.Contains(trimmed, "/runtime/") || strings.Contains(trimmed, "/testing/") {
				continue
			}
			if function != "" {
				signature = append(signature, normalizeLine(function))
			}
			signature = append(signature, match[1]+":"+match[2])
			siteLocated = true
			continue
		}
		if len(signature) > 1 {
			function = trimmed
		}
	}
	return strings.Join(signature, " | "), true
}

func failureBoilerplate(line string) bool {
	if line == "" {
		return true
	}
	for _, prefix := range []string{
		"=== RUN", "=== PAUSE", "=== CONT", "--- FAIL:", "--- PASS:",
		"FAIL", "PASS", "exit status ",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return strings.HasPrefix(line, "goroutine ") && strings.HasSuffix(line, "[running]:")
}

func normalizeLine(line string) string {
	line = strings.TrimSpace(line)
	line = leadingSourceLocation.ReplaceAllString(line, "")
	line = sourceLocation.ReplaceAllString(line, "$1:$2")
	line = volatileTestFlagValue.ReplaceAllString(line, "${1}${2}<value>")
	line = volatileTimestamp.ReplaceAllString(line, "<time>")
	line = volatileUUID.ReplaceAllString(line, "<uuid>")
	line = volatileAddress.ReplaceAllString(line, "<addr>")
	line = volatileGoroutine.ReplaceAllString(line, "goroutine <id>")
	line = volatileDuration.ReplaceAllString(line, "<duration>")
	line = volatileTempSuffix.ReplaceAllString(line, "${1}<rand>")
	line = volatileHexSegment.ReplaceAllString(line, "${1}<hash>")
	return strings.Join(strings.Fields(line), " ")
}
