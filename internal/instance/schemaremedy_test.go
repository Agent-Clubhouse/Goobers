package instance

import (
	"strings"
	"testing"
)

func TestRemedyInstanceSchemaVersion(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantChanged bool
		wantErr     string
		wantOut     string
	}{
		{
			name: "inserts the line after kind",
			in: `apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
runners:
  - name: self
    host: self
`,
			wantChanged: true,
			wantOut: `apiVersion: goobers.dev/v1alpha1
kind: Instance
schemaVersion: 2
repos: []
runners:
  - name: self
    host: self
`,
		},
		{
			name: "comments and ordering survive",
			// The whole reason this is a text transform: an operator's
			// instance.yaml is hand-edited and commented, and a
			// parse-and-remarshal remedy would silently discard all of it.
			in: `# managed by the platform team — do not reorder
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []       # filled in by bootstrap
runners:
  # the daemon's own pod
  - name: self
    host: self
`,
			wantChanged: true,
			wantOut: `# managed by the platform team — do not reorder
apiVersion: goobers.dev/v1alpha1
kind: Instance
schemaVersion: 2
repos: []       # filled in by bootstrap
runners:
  # the daemon's own pod
  - name: self
    host: self
`,
		},
		{
			name: "no runners: is left alone",
			in: `apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
runner:
  capabilities: [go@1.26]
`,
			wantChanged: false,
		},
		{
			name: "already correct is a no-op",
			in: `apiVersion: goobers.dev/v1alpha1
kind: Instance
schemaVersion: 2
repos: []
runners:
  - name: self
    host: self
`,
			wantChanged: false,
		},
		{
			name: "a present-but-wrong schemaVersion is reported, not rewritten",
			in: `apiVersion: goobers.dev/v1alpha1
kind: Instance
schemaVersion: 1
repos: []
runners:
  - name: self
    host: self
`,
			wantErr: "explicit value, not a missing line",
		},
		{
			name: "a nested runners: key is not the inventory",
			// `runners` here is indented under another key, so the file
			// declares no top-level inventory and needs no schemaVersion.
			// Matching it would insert a line into a file that never needed
			// one — a spurious edit to an operator-owned file.
			in: `apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
telemetry:
  runners: ignored
`,
			wantChanged: false,
		},
		{
			name: "runners: inside a block scalar is not the inventory",
			in: `apiVersion: goobers.dev/v1alpha1
kind: Instance
description: |
  runners:
    - name: not-really
repos: []
`,
			wantChanged: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := RemedyInstanceSchemaVersion(test.in)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want mention of %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Changed != test.wantChanged {
				t.Fatalf("Changed = %v, want %v (note: %s)", got.Changed, test.wantChanged, got.Note)
			}
			want := test.wantOut
			if !test.wantChanged {
				want = test.in
			}
			if got.After != want {
				t.Errorf("After =\n%s\nwant\n%s", got.After, want)
			}
			if got.Note == "" {
				t.Error("Note is empty; the CLI has nothing to print")
			}
		})
	}
}

// TestRemedyInstanceSchemaVersionOutputLoads closes the loop the acceptance
// criteria describe: the remedy's output is not merely well-formed text, it is
// a config the strict loader now ACCEPTS. Asserting the diff alone would pass
// even if the inserted line landed somewhere the parser ignores.
func TestRemedyInstanceSchemaVersionOutputLoads(t *testing.T) {
	const refused = `apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
runners:
  - name: self
    host: self
    provides:
      capabilities: [go@1.26]
`
	if _, err := LoadConfig(writeInstanceYAML(t, refused)); err == nil {
		t.Fatal("precondition failed: the input already loads, so the remedy proves nothing")
	}
	got, err := RemedyInstanceSchemaVersion(refused)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Changed {
		t.Fatal("remedy made no change to a config the loader refuses")
	}
	if _, err := LoadConfig(writeInstanceYAML(t, got.After)); err != nil {
		t.Fatalf("remedied config is still refused: %v", err)
	}
}
