//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testModule = "example.com/architecture"

func TestCheckAllowsReviewedDirectAndTransitiveImports(t *testing.T) {
	root := syntheticModule(t, map[string]string{
		"internal/pilot/pilot.go": `package pilot
import _ "example.com/architecture/internal/allowed"
`,
		"internal/allowed/allowed.go": `package allowed
import _ "example.com/architecture/internal/leaf"
`,
		"internal/leaf/leaf.go": "package leaf\n",
	})
	cfg := testConfig()

	message, err := check(context.Background(), root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message, "1 accepted pilot lane") {
		t.Fatalf("message = %q", message)
	}
}

func TestCheckRejectsForbiddenImportChains(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		tags      []string
		wantChain string
	}{
		{
			name: "direct",
			files: map[string]string{
				"internal/pilot/pilot.go": `package pilot
import _ "example.com/architecture/internal/engine"
`,
				"internal/engine/engine.go": "package engine\n",
			},
			wantChain: "internal/pilot -> example.com/architecture/internal/engine",
		},
		{
			name: "transitive",
			files: map[string]string{
				"internal/pilot/pilot.go": `package pilot
import _ "example.com/architecture/internal/allowed"
`,
				"internal/allowed/allowed.go": `package allowed
import _ "example.com/architecture/internal/engine"
`,
				"internal/engine/engine.go": "package engine\n",
			},
			wantChain: "internal/allowed -> example.com/architecture/internal/engine",
		},
		{
			name: "test-only backedge",
			files: map[string]string{
				"internal/pilot/pilot.go": "package pilot\n",
				"internal/pilot/pilot_test.go": `package pilot_test
import (
	_ "example.com/architecture/internal/engine"
	"testing"
)
func TestBoundary(t *testing.T) {}
`,
				"internal/engine/engine.go": "package engine\n",
			},
			wantChain: "internal/pilot_test -> example.com/architecture/internal/engine",
		},
		{
			name: "build-tag variant",
			files: map[string]string{
				"internal/pilot/pilot.go": "package pilot\n",
				"internal/pilot/tagged.go": `//go:build boundarytest

package pilot

import _ "example.com/architecture/internal/engine"
`,
				"internal/engine/engine.go": "package engine\n",
			},
			tags:      []string{"boundarytest"},
			wantChain: "internal/pilot -> example.com/architecture/internal/engine",
		},
		{
			name: "other pilot",
			files: map[string]string{
				"internal/pilot/pilot.go": `package pilot
import _ "example.com/architecture/internal/other"
`,
				"internal/other/other.go": "package other\n",
			},
			wantChain: "pilot-to-pilot import example.com/architecture/internal/other",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := syntheticModule(t, tc.files)
			cfg := testConfig()
			cfg.Builds[0].Tags = tc.tags

			_, err := check(context.Background(), root, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantChain) {
				t.Fatalf("error = %v, want actionable chain containing %q", err, tc.wantChain)
			}
		})
	}
}

func TestCheckRejectsUnreviewedDirectAndTransitiveImports(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name: "direct",
			files: map[string]string{
				"internal/pilot/pilot.go": `package pilot
import _ "example.com/architecture/internal/unreviewed"
`,
				"internal/unreviewed/unreviewed.go": "package unreviewed\n",
			},
			want: "unreviewed direct import example.com/architecture/internal/unreviewed",
		},
		{
			name: "transitive",
			files: map[string]string{
				"internal/pilot/pilot.go": `package pilot
import _ "example.com/architecture/internal/allowed"
`,
				"internal/allowed/allowed.go": `package allowed
import _ "example.com/architecture/internal/unreviewed"
`,
				"internal/unreviewed/unreviewed.go": "package unreviewed\n",
			},
			want: "unreviewed transitive import example.com/architecture/internal/unreviewed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := syntheticModule(t, tc.files)
			_, err := check(context.Background(), root, testConfig())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCheckFailsClosedOnMissingOrInvalidGraph(t *testing.T) {
	t.Run("missing namespace", func(t *testing.T) {
		root := syntheticModule(t, map[string]string{
			"unrelated/unrelated.go": "package unrelated\n",
		})

		_, err := check(context.Background(), root, testConfig())
		if err == nil || !strings.Contains(err.Error(), "package discovery failed") ||
			!strings.Contains(err.Error(), "example.com/architecture/internal/pilot") {
			t.Fatalf("error = %v, want missing namespace discovery diagnostic", err)
		}
	})

	t.Run("invalid dependency", func(t *testing.T) {
		root := syntheticModule(t, map[string]string{
			"internal/pilot/pilot.go": `package pilot
import _ "example.com/architecture/internal/missing"
`,
		})

		_, err := check(context.Background(), root, testConfig())
		if err == nil || !strings.Contains(err.Error(), "package discovery failed along") ||
			!strings.Contains(err.Error(), "example.com/architecture/internal/missing") {
			t.Fatalf("error = %v, want dependency discovery chain", err)
		}
	})
}

func TestCheckReportsPendingAndDeclinedLanesHonestly(t *testing.T) {
	cfg := testConfig()
	cfg.Lanes[0].Decision = "pending"

	message, err := check(context.Background(), t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message, "pending lane(s) remain preregistered") {
		t.Fatalf("message = %q", message)
	}

	cfg.Lanes[0].Decision = "declined"
	cfg.Lanes[0].Maintainer = "maintainer@example"
	cfg.Lanes[1].Decision = "declined"
	cfg.Lanes[1].Maintainer = "maintainer@example"
	message, err = check(context.Background(), t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message, "real-package checking is inapplicable") {
		t.Fatalf("message = %q", message)
	}
}

func TestLoadConfigRejectsMalformedConfiguration(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown field",
			body: `{"module":"example.com/x","builds":[],"lanes":[],"surprise":true}`,
			want: "unknown field",
		},
		{
			name: "no graph contexts",
			body: `{"module":"example.com/x","builds":[],"lanes":[{"path":"example.com/x/pilot","decision":"pending","rationale":"planned"}]}`,
			want: "at least one build context",
		},
		{
			name: "declined without maintainer",
			body: `{"module":"example.com/x","builds":[{"name":"host","goos":"linux","goarch":"amd64"}],"lanes":[{"path":"example.com/x/pilot","decision":"declined","rationale":"not cohesive"}]}`,
			want: "requires a named maintainer",
		},
		{
			name: "forbidden allowance",
			body: `{"module":"example.com/x","builds":[{"name":"host","goos":"linux","goarch":"amd64"}],"forbidden":[{"path":"example.com/x/internal/engine","rationale":"composition"}],"lanes":[{"path":"example.com/x/internal/pilot","decision":"accepted","rationale":"domain","allowedDirect":["example.com/x/internal/engine"],"allowedTransitive":["example.com/x/internal/engine"]}]}`,
			want: "overlaps forbidden boundary",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rules.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadConfig(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func testConfig() config {
	return config{
		Module: testModule,
		Builds: []buildContext{{
			Name:   "host",
			GOOS:   runtime.GOOS,
			GOARCH: runtime.GOARCH,
		}},
		Forbidden: []boundary{{
			Path:      testModule + "/internal/engine",
			Rationale: "composition stays outside domains",
		}},
		Lanes: []lane{
			{
				Path:              testModule + "/internal/pilot",
				Decision:          "accepted",
				Rationale:         "synthetic pilot",
				AllowedDirect:     []string{testModule + "/internal/allowed"},
				AllowedTransitive: []string{testModule + "/internal/allowed", testModule + "/internal/leaf"},
			},
			{
				Path:      testModule + "/internal/other",
				Decision:  "pending",
				Rationale: "synthetic second pilot",
			},
		},
	}
}

func syntheticModule(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files["go.mod"] = "module " + testModule + "\n\ngo 1.26.6\n"
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
