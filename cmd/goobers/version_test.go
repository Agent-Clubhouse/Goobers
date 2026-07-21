package main

import (
	"encoding/json"
	"testing"

	"github.com/goobers/goobers/internal/version"
)

// TestVersionJSON asserts `version --json` (and the `--version --json` alias)
// emit the structured version.Info object.
func TestVersionJSON(t *testing.T) {
	want := version.Get()
	for _, args := range [][]string{
		{"version", "--json"},
		{"--version", "--json"},
	} {
		t.Run(args[0], func(t *testing.T) {
			code, stdout, stderr := runArgs(t, args...)
			if code != 0 {
				t.Fatalf("code = %d, want 0; stderr = %q", code, stderr)
			}
			var got version.Info
			if err := json.Unmarshal([]byte(stdout), &got); err != nil {
				t.Fatalf("output is not valid JSON: %v\n%s", err, stdout)
			}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			if stderr != "" {
				t.Fatalf("stderr = %q, want empty", stderr)
			}
		})
	}
}

// TestVersionRejectsExtraArg keeps the flag surface tight.
func TestVersionRejectsExtraArg(t *testing.T) {
	code, _, _ := runArgs(t, "version", "bogus")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}
