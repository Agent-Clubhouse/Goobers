package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// numberWords maps the counts the prose plausibly spells out. The guide said
// "six" for a seven-capability set for two months after provider:ci:cancel
// joined it (#4517), and the design page said the same.
var numberWords = map[int]string{
	5: "five", 6: "six", 7: "seven", 8: "eight", 9: "nine", 10: "ten",
}

// docsRestatingDaemonIdentityCapabilities are the pages that spell out how many
// capabilities a configured daemonIdentity backs. Each must name every
// capability in daemonIdentityCapabilities and use the matching number word.
var docsRestatingDaemonIdentityCapabilities = []string{
	filepath.Join("..", "..", "docs", "guides", "github-token-scopes.md"),
	filepath.Join("..", "..", "docs", "design", "daemon-identity-multi-owner.md"),
}

func TestDaemonIdentityCapabilityDocsMatchTheGrantedSet(t *testing.T) {
	t.Parallel()

	want, ok := numberWords[len(daemonIdentityCapabilities)]
	if !ok {
		t.Fatalf("daemonIdentityCapabilities has %d entries; add its number word to numberWords", len(daemonIdentityCapabilities))
	}

	for _, path := range docsRestatingDaemonIdentityCapabilities {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		doc := string(raw)
		name := filepath.Base(path)

		for _, capability := range daemonIdentityCapabilities {
			if !strings.Contains(doc, "`"+string(capability)+"`") {
				t.Errorf("%s does not name daemon-identity capability %q", name, capability)
			}
		}

		// Reject a stale spelled-out count. Only flag a wrong word that is
		// actually adjacent to the capability discussion, so unrelated prose
		// elsewhere in these long pages cannot trip the check.
		for count, word := range numberWords {
			if count == len(daemonIdentityCapabilities) {
				continue
			}
			for _, phrase := range []string{
				fmt.Sprintf("the %s capabilities", word),
				fmt.Sprintf("%s capabilities: `repo:push`", word),
			} {
				if strings.Contains(doc, phrase) {
					t.Errorf("%s says %q but daemonIdentityCapabilities has %d entries (want %q)", name, phrase, len(daemonIdentityCapabilities), want)
				}
			}
		}
	}
}
