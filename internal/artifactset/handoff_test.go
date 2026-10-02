package artifactset

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/handoffcheck"
)

func handoffIssue(code string) handoffcheck.Issue { return handoffcheck.Issue{Code: code} }

func prepareJSON(t *testing.T, body string) error {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, root, []ManifestEntry{{Name: "verdict.json", Path: "payload", MediaType: "application/json"}})
	_, err := Prepare(context.Background(), root, "manifest.json", func(_ string, data []byte) ([]byte, error) { return data, nil })
	return err
}

func TestPrepareClassifiesJSONHandoffFailures(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"truncated", `{"a":[1,2`, "truncated"},
		{"duplicate key", `{"a":1,"a":2}`, "duplicate_key at /a"},
		{"code fence", "```json\n{}\n```", "not_json_document"},
		{"trailing data", `{} {}`, "trailing_data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := prepareJSON(t, tc.body)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
			he, ok := AsHandoffError(err)
			if !ok || he.Entry != "verdict.json" {
				t.Fatalf("want HandoffError, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message %q missing %q", err.Error(), tc.want)
			}
		})
	}
}

func TestPrepareAcceptsValidJSONHandoff(t *testing.T) {
	if err := prepareJSON(t, `{"verdict":"pass","items":[1,2]}`); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffErrorDoesNotEchoPayloadValues(t *testing.T) {
	err := prepareJSON(t, `{"token":"hunter2-value","token":"x"}`)
	if err == nil || strings.Contains(err.Error(), "hunter2-value") {
		t.Fatalf("payload value leaked or no error: %v", err)
	}
}

func TestHandoffErrorCapsIssueList(t *testing.T) {
	he := &HandoffError{Entry: "x"}
	for i := 0; i < 9; i++ {
		he.Issues = append(he.Issues, handoffIssue("schema_violation"))
	}
	if !strings.Contains(he.Error(), "and 4 more") {
		t.Fatalf("issues not capped: %s", he.Error())
	}
}
