package creditgraph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// entryPoints are the package's two consumer-facing entry points. While both
// sit in the deadcode exemption ledger, nothing in production builds or
// attributes a credit graph, and docs/design/credit-graph.md must say so.
var entryPoints = []string{
	"github.com/goobers/goobers/internal/creditgraph.Build",
	"github.com/goobers/goobers/internal/creditgraph.Attribute",
}

// noProductionCallerClaim is the claim in the design's status banner that this
// test keeps honest in both directions. It is matched against the page with
// blockquote markers and line wrapping normalised away, so reflowing the
// paragraph cannot silently disarm the check.
const noProductionCallerClaim = "no production caller"

// normalizeProse strips Markdown blockquote markers and collapses all runs of
// whitespace to single spaces so a claim can be matched regardless of wrapping.
func normalizeProse(markdown string) string {
	var b strings.Builder
	for _, line := range strings.Split(markdown, "\n") {
		b.WriteString(strings.TrimPrefix(strings.TrimSpace(line), "> "))
		b.WriteString(" ")
	}
	return strings.Join(strings.Fields(strings.ReplaceAll(b.String(), "**", "")), " ")
}

// The design page claimed a landed read model for months while production
// attribution ran entirely through internal/readmodel (#4523). Tie the claim to
// the mechanical evidence: an exempted entry point means unreachable, so the
// design must say so; wiring one up must force the design to be rewritten in
// the same change rather than left overstating or understating integration.
func TestDesignStatusMatchesDeadcodeReachability(t *testing.T) {
	t.Parallel()

	repoRoot := filepath.Join("..", "..")

	exemptionsRaw, err := os.ReadFile(filepath.Join(repoRoot, "test", "deadcode", "exemptions.txt"))
	if err != nil {
		t.Fatalf("read deadcode exemptions: %v", err)
	}
	exemptions := string(exemptionsRaw)

	var exempted []string
	for _, symbol := range entryPoints {
		if strings.Contains(exemptions, symbol+" ") || strings.Contains(exemptions, symbol+"\n") {
			exempted = append(exempted, symbol)
		}
	}

	designRaw, err := os.ReadFile(filepath.Join(repoRoot, "docs", "design", "credit-graph.md"))
	if err != nil {
		t.Fatalf("read credit-graph design: %v", err)
	}
	designClaimsUnwired := strings.Contains(normalizeProse(string(designRaw)), noProductionCallerClaim)

	switch {
	case len(exempted) == len(entryPoints) && !designClaimsUnwired:
		t.Errorf("both credit-graph entry points are deadcode-exempt (unreachable from production), but docs/design/credit-graph.md no longer says they have no production caller; restore that status or wire them up")
	case len(exempted) == 0 && designClaimsUnwired:
		t.Errorf("credit-graph entry points are reachable from production, but docs/design/credit-graph.md still says they have no production caller; update the status and the #4523 disposition")
	case len(exempted) > 0 && len(exempted) < len(entryPoints):
		t.Errorf("credit-graph entry points are half-wired (%v still exempt); docs/design/credit-graph.md describes an all-or-nothing status, so record the partial integration explicitly", exempted)
	}
}
