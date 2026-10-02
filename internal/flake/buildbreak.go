package flake

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// buildBreakMessageLimit bounds how many distinct compiler diagnostics a
// build-break signature carries. The Go compiler itself stops after ten.
const buildBreakMessageLimit = 10

var (
	// compileHeader is the "# import/path" line the go tool prints before the
	// diagnostics of a package that failed to build or vet.
	compileHeader = regexp.MustCompile(`^#\s+\S+`)
	// compileDiagnostic is a compiler diagnostic: file.go:line:column: message.
	// The column is what separates it from a t.Log/t.Error line, which the
	// testing package prints as file.go:line: message.
	compileDiagnostic = regexp.MustCompile(`^(?:.*[\\/])?([^\s\\/:]+\.go):\d+:\d+:\s+(.+)$`)
)

// BuildBreakSignature reports the normalized compile-error signature of a
// failure's output, or false when the output is not a compile failure.
//
// One commit that breaks a shared package fails every downstream package that
// builds it, each under its own package, test name and wrapper message
// (#4230). The signature keeps only what those failures share — the source
// file base name and the compiler's message — and drops the package path,
// relative path, line and column that differ from one consumer to the next,
// so the same break observed from many packages yields one signature.
func BuildBreakSignature(text string) (string, bool) {
	headed := false
	seen := make(map[string]bool)
	var diagnostics []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if compileHeader.MatchString(line) {
			headed = true
			continue
		}
		if !headed {
			continue
		}
		match := compileDiagnostic.FindStringSubmatch(line)
		if len(match) != 3 {
			continue
		}
		diagnostic := match[1] + ": " + normalizeLine(match[2])
		if seen[diagnostic] {
			continue
		}
		seen[diagnostic] = true
		diagnostics = append(diagnostics, diagnostic)
	}
	if len(diagnostics) == 0 {
		return "", false
	}
	slices.Sort(diagnostics)
	if len(diagnostics) > buildBreakMessageLimit {
		diagnostics = diagnostics[:buildBreakMessageLimit]
	}
	return boundSignature("build failed: " + strings.Join(diagnostics, " | ")), true
}

// BuildBreakFingerprint returns the ledger identity of one build break: the
// commit it was observed at plus its normalized compile-error signature. The
// leading domain tag keeps it apart from per-package Fingerprint identities.
func BuildBreakFingerprint(sha, signature string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte("build-break\x00"+strings.TrimSpace(sha)+"\x00"+signature)))
}
