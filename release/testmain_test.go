package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// releaseDocsGeneratorFixtureEnv marks a re-exec of this test binary as the
// stand-in release docs generator (see installTestDocsGenerator).
const releaseDocsGeneratorFixtureEnv = "GOOBERS_RELEASE_TEST_DOCS_GENERATOR"

// releaseDocsGeneratorFixtureMarker is the content every fixture-generated doc
// carries, so a test can prove the generator's output (not some committed
// docs/ file) is what was staged into the archive.
const releaseDocsGeneratorFixtureMarker = "release docs generator fixture"

// TestMain replaces the release docs generator build for the whole package.
//
// run() stages the packaged docs by building ./cmd/goobers and invoking its
// hidden `__generate-docs` command (buildReleaseDocsGenerator). Every test
// that reaches run() therefore paid for a full, uncached `go build` of the
// CLI — minutes per test job, since that build shares neither the -race nor
// the coverage build cache of the test binary itself. No test here asserts on
// the CLI's real generated docs: what they protect is that run() builds a
// generator, invokes `__generate-docs <dir>` as a separate process, and stages
// its output. So the generator is this test binary, copied into place and
// re-dispatched below to write a small fixture tree.
//
// The real build is covered where it is load-bearing: the release workflow
// packages with `go run ./release` (the real buildReleaseDocsGenerator) and then
// diffs the installed binary's `__generate-docs` output against the packaged
// docs, and cmd/goobers' TestCLIDocsGeneratorContract drives the real
// `__generate-docs` command in a separate process.
func TestMain(m *testing.M) {
	if os.Getenv(releaseDocsGeneratorFixtureEnv) == "1" && len(os.Args) > 1 && os.Args[1] == "__generate-docs" {
		os.Exit(runFixtureDocsGenerator(os.Args[2:], os.Stderr))
	}
	if err := os.Setenv(releaseDocsGeneratorFixtureEnv, "1"); err != nil {
		fmt.Fprintf(os.Stderr, "set %s: %v\n", releaseDocsGeneratorFixtureEnv, err)
		os.Exit(1)
	}
	releaseDocsGeneratorBuilder = installTestDocsGenerator
	os.Exit(m.Run())
}

// installTestDocsGenerator is the package-wide releaseDocsGeneratorBuilder: it
// copies this test binary to output instead of compiling ./cmd/goobers. It
// copies rather than links so the generator's image lock on Windows is
// independent of the running test binary's.
func installTestDocsGenerator(_, output, _ string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve test executable: %w", err)
	}
	src, err := os.Open(executable)
	if err != nil {
		return fmt.Errorf("open test executable: %w", err)
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create test docs generator: %w", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return fmt.Errorf("copy test docs generator: %w", err)
	}
	return dst.Close()
}

// runFixtureDocsGenerator is the re-exec'd generator: it honors the same
// `__generate-docs <docs-directory>` contract as the real CLI command and
// writes the CLI reference, completions and man page the archive must carry.
func runFixtureDocsGenerator(args []string, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: docs-generator __generate-docs <docs-directory>")
		return 2
	}
	files := map[string]string{
		"cli/README.md":           "# " + releaseDocsGeneratorFixtureMarker + "\n",
		"completion/goobers.bash": "# " + releaseDocsGeneratorFixtureMarker + "\n",
		"completion/goobers.fish": "# " + releaseDocsGeneratorFixtureMarker + "\n",
		"completion/_goobers":     "# " + releaseDocsGeneratorFixtureMarker + "\n",
		"man/goobers.1":           ".TH GOOBERS 1\n\\\" " + releaseDocsGeneratorFixtureMarker + "\n",
	}
	for rel, content := range files {
		path := filepath.Join(args[0], filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	}
	return 0
}
