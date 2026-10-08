package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// buildMetadataVars are the Makefile variables stamped into internal/version via
// -ldflags, with the fallback each one reports when git cannot be queried.
var buildMetadataVars = map[string]string{
	"VERSION": "dev",
	"COMMIT":  "none",
	"DATE":    "unknown",
}

// posixOnlyShellSyntax is any shell syntax that forces GNU Make to run a
// $(shell ...) through SHELL. On a shell-less Windows make that is cmd.exe,
// which rejects `2>/dev/null` and mangles `%`, so the fallback silently wins.
var posixOnlyShellSyntax = []string{"/dev/null", "||", "&&", ">", "<", "|", ";", "'", "\"", "`", "$$("}

// TestMakefileBuildMetadataAvoidsShellSyntax pins the Makefile's git probes to
// plain commands (#5388) so Make can exec git directly on every platform, with
// the fallback applied by Make rather than by a shell `||`.
func TestMakefileBuildMetadataAvoidsShellSyntax(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	makefile := string(data)
	for name, fallback := range buildMetadataVars {
		pattern := regexp.MustCompile(`(?m)^` + name + `\s*\?=\s*(.*)$`)
		match := pattern.FindStringSubmatch(makefile)
		if match == nil {
			t.Errorf("Makefile has no `%s ?=` build-metadata definition", name)
			continue
		}
		value := strings.TrimSpace(match[1])
		want := "$(or $(shell git "
		if !strings.HasPrefix(value, want) || !strings.HasSuffix(value, "),"+fallback+")") {
			t.Errorf("%s = %q; want %s...),%s) so Make, not a shell, applies the fallback", name, value, want, fallback)
		}
		for _, syntax := range posixOnlyShellSyntax {
			if strings.Contains(value, syntax) {
				t.Errorf("%s = %q contains shell syntax %q; it routes git through SHELL (cmd.exe on Windows) and loses provenance", name, value, syntax)
			}
		}
	}
	if !strings.Contains(makefile, "$(warning build provenance unavailable") {
		t.Error("Makefile no longer warns when build provenance falls back to unidentifiable values")
	}
}

// TestMakefileBuildMetadataResolvesFromGit evaluates the Makefile with the host
// make and checks that a git checkout yields real commit/date metadata instead
// of the silent fallbacks.
func TestMakefileBuildMetadataResolvesFromGit(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Skip("not a git checkout; build metadata legitimately falls back")
	}
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cmd := exec.Command(makeBin, "-s", "--no-print-directory",
		"--eval=print-build-metadata: ; @echo [$(VERSION)] [$(COMMIT)] [$(DATE)]", "print-build-metadata")
	cmd.Dir = root
	cmd.Env = withoutBuildMetadataEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make print-build-metadata: %v\n%s", err, out)
	}
	got := string(out)
	if strings.Contains(got, "build provenance unavailable") {
		t.Errorf("make warned about lost provenance in a git checkout:\n%s", got)
	}
	want := regexp.MustCompile(`\[([^\]\s]+)\] \[([0-9a-f]{4,})\] \[\d{4}-\d{2}-\d{2}T[^\]]+\]`)
	match := want.FindStringSubmatch(got)
	if match == nil {
		t.Fatalf("make resolved build metadata %q; want a git version, commit and ISO commit date", strings.TrimSpace(got))
	}
	// `git describe --always` yields a tag or embeds the abbreviated commit; it
	// never yields the `dev` fallback.
	if version := match[1]; version == buildMetadataVars["VERSION"] {
		t.Errorf("make resolved VERSION to the %q fallback in a git checkout", version)
	}
}

// withoutBuildMetadataEnv drops inherited VERSION/COMMIT/DATE so the Makefile's
// `?=` defaults, not the caller's environment, are what gets evaluated.
func withoutBuildMetadataEnv(env []string) []string {
	kept := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if _, ok := buildMetadataVars[strings.ToUpper(name)]; ok {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}
