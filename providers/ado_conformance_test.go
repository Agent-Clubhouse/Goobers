package providers

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// adoVoteKeyPattern matches a JSON "vote" key written from Go source, along
// with whatever follows the colon up to the end of the value expression.
var adoVoteKeyPattern = regexp.MustCompile(`"vote"\s*:\s*([^,}\n]*)`)

// adoVoteZeroPattern is the only vote value Goobers may write: 0, "no vote".
var adoVoteZeroPattern = regexp.MustCompile(`^0\s*$`)

// adoVoteTagPattern is the read-side struct tag decoding a reviewer's vote.
var adoVoteTagPattern = regexp.MustCompile("json:\"vote\"")

// TestADONeverBypassesPolicyOrVotes machine-enforces the ADO branch-policy
// invariant in docs/design/ado-parity-dsl-2-0.md §5 (ADO-N9): Goobers never
// asks the server to skip branch policies, and never casts a non-zero
// reviewer vote (approving its own pull request would defeat a
// required-reviewer policy on some configurations). It scans every non-test
// source file in this package.
func TestADONeverBypassesPolicyOrVotes(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob sources: %v", err)
	}
	scanned, zeroVoteWrites := 0, 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		scanned++
		violations, zero := scanADOPolicyConformance(path, string(data))
		for _, v := range violations {
			t.Error(v)
		}
		zeroVoteWrites += zero
	}
	// Loud preconditions: a scan that read nothing, or no longer sees the one
	// known vote write (RequestReview's vote: 0), would pass vacuously.
	if scanned == 0 {
		t.Fatal("scanned no provider sources; the conformance scan is not running where it should")
	}
	if zeroVoteWrites == 0 {
		t.Fatal(`found no "vote": 0 write; the vote scan no longer matches how votes are written, so update it`)
	}
}

// scanADOPolicyConformance returns every forbidden construct in src and how
// many permitted "vote": 0 writes it saw.
func scanADOPolicyConformance(path, src string) (violations []string, zero int) {
	for n, line := range strings.Split(src, "\n") {
		lower := strings.ToLower(line)
		for _, forbidden := range []string{"bypasspolicy", "bypassreason"} {
			if strings.Contains(lower, forbidden) {
				violations = append(violations, fmt.Sprintf("%s:%d mentions %s; Goobers must never bypass ADO branch policies", path, n+1, forbidden))
			}
		}
		if adoVoteTagPattern.MatchString(line) {
			continue
		}
		for _, m := range adoVoteKeyPattern.FindAllStringSubmatch(line, -1) {
			if adoVoteZeroPattern.MatchString(m[1]) {
				zero++
				continue
			}
			violations = append(violations, fmt.Sprintf("%s:%d writes vote %q; Goobers may only write vote 0", path, n+1, strings.TrimSpace(m[1])))
		}
	}
	return violations, zero
}

// TestADOPolicyConformanceScanDetectsViolations proves the scanner itself
// flags each forbidden shape, so the conformance test cannot pass by
// matching nothing.
func TestADOPolicyConformanceScanDetectsViolations(t *testing.T) {
	cases := map[string]string{
		"bypass flag":   `body["bypassPolicy"] = true`,
		"bypass reason": `opts.BypassReason = "x"`,
		"approve vote":  `map[string]int{"vote": 10}`,
		"dynamic vote":  `map[string]int{"vote": v}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if violations, _ := scanADOPolicyConformance("probe.go", src); len(violations) == 0 {
				t.Fatalf("scanner accepted %q", src)
			}
		})
	}
	for _, src := range []string{`map[string]int{"vote": 0}`, "Vote int `json:\"vote\"`"} {
		violations, _ := scanADOPolicyConformance("probe.go", src)
		if len(violations) != 0 {
			t.Fatalf("scanner rejected permitted %q: %v", src, violations)
		}
	}
}
