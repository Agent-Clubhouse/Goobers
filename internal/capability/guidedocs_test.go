package capability

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// capabilityRow matches the first column of a Markdown table row whose cell is
// a single backticked identifier shaped like a capability ("resource:verb" or
// "resource:sub:verb"), optionally followed by a parenthetical qualifier.
//
// The capability table is where an operator reads which PAT permissions to
// mint, so a row naming an identifier the registry does not know provisions
// access for a grant no stage can ever declare. The guide shipped a
// `repo:clone` row for months; config load rejects that name (#4519).
var capabilityRow = regexp.MustCompile("(?m)^\\|\\s*`([a-z][a-z0-9-]*(?::[a-z0-9-]+){1,2})`[^|]*\\|")

// tokenScopesGuide is the operator-facing GitHub credential contract.
const tokenScopesGuide = "github-token-scopes.md"

// adoNamespace capabilities are documented by the Azure DevOps auth guide, not
// this GitHub-specific page, so the completeness half of the check skips them.
const adoNamespace = "ado:"

func TestGitHubTokenScopesGuideMatchesCapabilityRegistry(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "docs", "guides", tokenScopesGuide)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", tokenScopesGuide, err)
	}
	guide := string(raw)

	rows := capabilityRow.FindAllStringSubmatch(guide, -1)
	if len(rows) == 0 {
		t.Fatalf("%s has no capability table rows; the guard would pass vacuously", tokenScopesGuide)
	}
	for _, row := range rows {
		name := row[1]
		if Known(name) {
			continue
		}
		hint := ""
		if suggestion, ok := Suggest(name); ok {
			hint = fmt.Sprintf(" (did you mean %q?)", suggestion)
		}
		t.Errorf("%s documents PAT permissions for %q, which is not a canonical capability%s; config load rejects it", tokenScopesGuide, name, hint)
	}

	for _, capability := range All() {
		name := string(capability)
		if strings.HasPrefix(name, adoNamespace) {
			continue
		}
		if !strings.Contains(guide, "`"+name+"`") {
			t.Errorf("%s has no row for capability %q; every non-ADO capability needs a documented PAT permission (or an explicit \"no GitHub permission\" note)", tokenScopesGuide, name)
		}
	}
}
