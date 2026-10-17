package publicationguard

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCheckPreservesShallowInspectionError(t *testing.T) {
	for _, want := range []error{context.Canceled, context.DeadlineExceeded, errors.New("inspection unavailable")} {
		t.Run(want.Error(), func(t *testing.T) {
			calls := 0
			git := func(context.Context, ...string) ([]byte, error) {
				calls++
				return nil, want
			}
			if tip, err := Check(t.Context(), git, "base", "head"); tip != "" || !errors.Is(err, want) || calls != 1 {
				t.Fatalf("publication tip=%q error=%v reads=%d, want refusal preserving %v", tip, err, calls, want)
			}
		})
	}
}

func TestCredentialScreen(t *testing.T) {
	for _, input := range []string{
		"ghs_" + strings.Repeat("A", 36),
		"github_pat_" + strings.Repeat("B", 50),
		"GH_TOKEN=" + strings.Repeat("C", 24),
		`"COPILOT_CONNECTION_TOKEN": "` + strings.Repeat("D", 24) + `"`,
		"'copilot_github_token': '" + strings.Repeat("E", 24) + "'",
	} {
		if !ContainsCredential([]byte(input)) {
			t.Fatal("credential-shaped synthetic fixture was accepted")
		}
	}
	for _, input := range []string{"ordinary code", "GH_TOKEN=$TOKEN", `os.Getenv("GH_TOKEN")`, "GH_TOKEN=[REDACTED]", ".goobers is reserved"} {
		if ContainsCredential([]byte(input)) {
			t.Fatalf("ordinary reference blocked: %q", input)
		}
	}
}

func TestCheckFailsClosedOnIncompleteInspection(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, test := range []struct {
		name, shallow, inventory, tree string
		fail                           string
	}{
		{name: "shallow", shallow: "true"},
		{name: "unknown depth", shallow: ""},
		{name: "revision failure", shallow: "false", fail: "rev-parse"},
		{name: "history failure", shallow: "false", fail: "rev-list"},
		{name: "malformed commit", shallow: "false", inventory: "not-a-commit"},
		{name: "tree failure", shallow: "false", inventory: sha, fail: "diff-tree"},
		{name: "malformed tree", shallow: "false", inventory: sha, tree: "invalid\x00"},
		{name: "blob failure", shallow: "false", inventory: sha, tree: ":000000 100644 " + strings.Repeat("0", 40) + " " + sha + " A\x00source.txt\x00", fail: "cat-file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			git := func(_ context.Context, args ...string) ([]byte, error) {
				if args[0] == "rev-parse" && args[1] == "--is-shallow-repository" {
					return []byte(test.shallow), nil
				}
				if args[0] == test.fail {
					return nil, errors.New("inspection unavailable")
				}
				switch args[0] {
				case "rev-parse":
					return []byte(sha), nil
				case "rev-list":
					return []byte(test.inventory), nil
				case "diff-tree":
					return []byte(test.tree), nil
				default:
					t.Fatalf("unexpected read %q", args[0])
					return nil, nil
				}
			}
			if _, err := Check(t.Context(), git, "base", "head"); err == nil {
				t.Fatal("incomplete inspection passed")
			}
		})
	}
}
