package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildReleaseTargetsConcurrentResultsKeepTargetOrder pins #5413's
// parallel build: targets compile concurrently, but archives, skip notices and
// build lines come back in the requested target order, so SHA256SUMS inputs and
// the log stay deterministic however the builds finish.
func TestBuildReleaseTargetsConcurrentResultsKeepTargetOrder(t *testing.T) {
	orig := buildPackage
	buildPackage = "./"
	defer func() { buildPackage = orig }()

	targets, err := parseTargets("linux/amd64,windows/ppc64,darwin/arm64,windows/amd64")
	if err != nil {
		t.Fatal(err)
	}
	opts := options{version: "v1.2.3", outDir: t.TempDir(), targets: targets, skipUnbuildable: true}
	docs := t.TempDir()
	if err := os.WriteFile(filepath.Join(docs, "README.md"), []byte("docs"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	archives, skipped, err := buildReleaseTargets(opts, "-s -w", docs, nil, &stdout)
	if err != nil {
		t.Fatalf("buildReleaseTargets: %v", err)
	}
	var names []string
	for _, archive := range archives {
		names = append(names, filepath.Base(archive))
	}
	want := []string{"goobers_v1.2.3_linux_amd64.tar.gz", "goobers_v1.2.3_darwin_arm64.tar.gz", "goobers_v1.2.3_windows_amd64.zip"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("archives = %v, want target order %v", names, want)
	}
	if strings.Join(skipped, ",") != "windows/ppc64" {
		t.Fatalf("skipped = %v, want [windows/ppc64]", skipped)
	}
	var reported []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if fields := strings.Fields(line); len(fields) > 1 {
			reported = append(reported, fields[1])
		}
	}
	if got := strings.Join(reported, ","); got != "linux/amd64,windows/ppc64,darwin/arm64,windows/amd64" {
		t.Fatalf("report order = %s, want target order:\n%s", got, stdout.String())
	}
	entries, err := os.ReadDir(opts.outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		t.Fatalf("output holds %d entries, want only the %d archives (binaries removed)", len(entries), len(want))
	}
}

// Without -skip-unbuildable the first unbuildable target in requested order is
// the one reported, even when a later target also fails.
func TestBuildReleaseTargetsReportsFirstFailureInTargetOrder(t *testing.T) {
	orig := buildPackage
	buildPackage = "./"
	defer func() { buildPackage = orig }()

	targets, err := parseTargets("linux/amd64,windows/ppc64,plan9/riscv64")
	if err != nil {
		t.Fatal(err)
	}
	opts := options{version: "v1.2.3", outDir: t.TempDir(), targets: targets}
	_, _, err = buildReleaseTargets(opts, "-s -w", t.TempDir(), nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "build windows/ppc64 failed") {
		t.Fatalf("err = %v, want the first failing target in order (windows/ppc64)", err)
	}
}
