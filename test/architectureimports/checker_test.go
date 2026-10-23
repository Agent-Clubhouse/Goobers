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
			name: "unreferenced nested package",
			files: map[string]string{
				"internal/pilot/pilot.go": "package pilot\n",
				"internal/pilot/nested/nested.go": `package nested
import _ "example.com/architecture/internal/engine"
`,
				"internal/engine/engine.go": "package engine\n",
			},
			wantChain: "internal/pilot/nested -> example.com/architecture/internal/engine",
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

func TestGoobersClosureExcludesCloudDeps(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	cfg, err := loadConfig(filepath.Join(root, "test", "architectureimports", "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkClosures(context.Background(), root, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestGoobersCloudClosureExcludesRunner(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	cfg, err := loadConfig(filepath.Join(root, "test", "architectureimports", "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rule := range cfg.ClosureRules {
		if rule.Name == "cloud-control-excludes-runner" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("cloud control runner exclusion is missing")
	}
	if err := checkClosures(context.Background(), root, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestGoobersClosureRejectsPGXFixture(t *testing.T) {
	root := syntheticModule(t, map[string]string{
		"go.mod": `module example.com/architecture

go 1.26.10

require github.com/jackc/pgx/v5 v5.0.0

replace github.com/jackc/pgx/v5 => ./third_party/pgx
`,
		"cmd/goobers/main.go": `package main
import _ "github.com/jackc/pgx/v5"
func main() {}
`,
		"third_party/pgx/go.mod": "module github.com/jackc/pgx/v5\n\ngo 1.26.10\n",
		"third_party/pgx/pgx.go": "package pgx\n",
	})
	cfg := config{
		Module: testModule,
		Builds: []buildContext{{
			Name: runtime.GOOS, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		}},
		ClosureRules: []closureRule{{
			Name:  "local",
			Roots: []string{testModule + "/cmd/goobers"},
			Forbidden: []boundary{{
				Path: "github.com/jackc/pgx", Rationale: "cloud database client",
			}},
		}},
	}
	err := checkClosures(context.Background(), root, cfg)
	if err == nil || !strings.Contains(err.Error(), "forbidden cloud dependency github.com/jackc/pgx/v5") ||
		!strings.Contains(err.Error(), "cmd/goobers -> github.com/jackc/pgx/v5") {
		t.Fatalf("error = %v, want pgx import chain", err)
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
	if _, supplied := files["go.mod"]; !supplied {
		files["go.mod"] = "module " + testModule + "\n\ngo 1.26.10\n"
	}
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

func TestCloudRunnerRatchetRejectsNewEdgesAndStaleExceptions(t *testing.T) {
	rule := closureRule{Name: "cloud", Roots: []string{"module/cloud"}, Forbidden: []boundary{{Path: "module/runner", Rationale: "D-2"}}, ReviewedEdges: []reviewedEdge{{From: "module/worker", To: "module/runner", Rationale: "existing worker; #7848"}}}
	graph := map[string]listedPackage{
		"module/cloud":  {Imports: []string{"module/worker", "module/engine"}},
		"module/worker": {Imports: []string{"module/runner"}},
		"module/engine": {}, "module/runner": {},
	}
	if err := checkClosure(rule, "module/cloud", graph); err != nil {
		t.Fatal(err)
	}
	graph["module/engine"] = listedPackage{Imports: []string{"module/runner"}}
	if err := checkClosure(rule, "module/cloud", graph); err == nil || !strings.Contains(err.Error(), "module/engine -> module/runner") {
		t.Fatalf("new edge was allowed: %v", err)
	}
	graph["module/engine"] = listedPackage{}
	graph["module/worker"] = listedPackage{}
	if err := checkClosure(rule, "module/cloud", graph); err == nil || !strings.Contains(err.Error(), "obsolete reviewed closure edge") {
		t.Fatalf("stale exception retained: %v", err)
	}
}
