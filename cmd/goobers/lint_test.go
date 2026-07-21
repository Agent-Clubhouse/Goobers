package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLintExitCodes locks the #439 acceptance contract: 0 = clean, 1 = findings,
// 2 = usage/IO error, driven through the full CLI dispatch (so registry wiring
// is exercised too).
func TestLintExitCodes(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "inst")
		if code, _, stderr := runArgs(t, "init", root); code != 0 {
			t.Fatalf("init: code=%d stderr=%q", code, stderr)
		}
		code, stdout, stderr := runArgs(t, "lint", root)
		if code != 0 {
			t.Fatalf("lint clean: code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "OK: instance.yaml valid") {
			t.Fatalf("lint clean stdout missing OK line:\n%s", stdout)
		}
	})

	t.Run("findings", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "inst")
		if code, _, stderr := runArgs(t, "init", root); code != 0 {
			t.Fatalf("init: code=%d stderr=%q", code, stderr)
		}
		// Same mutation the validate suite uses: point a workflow at a gaggle
		// that does not exist, which the shared engine rejects.
		path := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
		replaceInFile(t, path, "  gaggle: example", "  gaggle: ghost")
		code, _, _ := runArgs(t, "lint", root)
		if code != 1 {
			t.Fatalf("lint findings: code=%d, want 1", code)
		}
	})

	t.Run("not an instance", func(t *testing.T) {
		code, _, stderr := runArgs(t, "lint", t.TempDir())
		if code != 2 {
			t.Fatalf("lint non-instance: code=%d, want 2", code)
		}
		if !strings.Contains(stderr, "not an instance root") {
			t.Fatalf("stderr should hint at a missing instance root:\n%s", stderr)
		}
	})
}

// TestLintValidateParity is the load-bearing #439/#252 guarantee: `lint` and
// `validate` are one authoritative path, not two validators that can drift. For
// identical input they must produce byte-identical exit codes AND output —
// across clean, findings, and flag-carrying invocations. If a future change
// gives one verb a check the other lacks, this fails.
func TestLintValidateParity(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, root string)
		args   func(root string) []string
	}{
		{
			name: "clean",
			args: func(root string) []string { return []string{root} },
		},
		{
			name: "findings",
			mutate: func(t *testing.T, root string) {
				path := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
				replaceInFile(t, path, "  gaggle: example", "  gaggle: ghost")
			},
			args: func(root string) []string { return []string{root} },
		},
		{
			name: "source-tree flag",
			args: func(root string) []string { return []string{"--source-tree", filepath.Join(root, "config")} },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "inst")
			if code, _, stderr := runArgs(t, "init", root); code != 0 {
				t.Fatalf("init: code=%d stderr=%q", code, stderr)
			}
			if tc.mutate != nil {
				tc.mutate(t, root)
			}
			vCode, vOut, vErr := runArgs(t, append([]string{"validate"}, tc.args(root)...)...)
			lCode, lOut, lErr := runArgs(t, append([]string{"lint"}, tc.args(root)...)...)
			if vCode != lCode {
				t.Fatalf("exit code drift: validate=%d lint=%d", vCode, lCode)
			}
			if vOut != lOut {
				t.Fatalf("stdout drift between validate and lint:\nvalidate:\n%s\nlint:\n%s", vOut, lOut)
			}
			if vErr != lErr {
				t.Fatalf("stderr drift between validate and lint:\nvalidate:\n%s\nlint:\n%s", vErr, lErr)
			}
		})
	}
}

// TestLintHelpIdentity confirms the alias keeps its own `-h` identity: `lint`
// renders lint help (mentioning it is the shared/CI path), not validate's, even
// though they share one engine.
func TestLintHelpIdentity(t *testing.T) {
	code, _, stderr := runArgs(t, "lint", "--bogus-flag")
	if code != 2 {
		t.Fatalf("lint --bogus-flag: code=%d, want 2", code)
	}
	if !strings.Contains(stderr, "goobers lint") {
		t.Fatalf("lint usage should name the lint verb:\n%s", stderr)
	}
}
