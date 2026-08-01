package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRepositoryRejectsAnonymousFlakeSkipAndWorkflowRetry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFixture(t, root, "pkg/example_test.go", `package pkg
import "testing"
func TestExample(t *testing.T) { t.Skip("flaky under load") }
`)
	writeFixture(t, root, ".github/workflows/ci.yml", `name: CI
jobs:
  test:
    steps:
      - uses: nick-fields/retry@v3
`)
	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 2 {
		t.Fatalf("violations = %+v, want two", violations)
	}
	if !strings.Contains(violations[0].Message+violations[1].Message, "flake.Quarantine") ||
		!strings.Contains(violations[0].Message+violations[1].Message, "workflow retries") {
		t.Fatalf("violations = %+v", violations)
	}
}

func TestCheckWorkflowRejectsCommonRetryForms(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"retries: 3",
		"max_attempts: 3",
		"uses: Wandalen/wretry.action@v3",
	} {
		root := t.TempDir()
		writeFixture(t, root, ".github/workflows/ci.yml", "jobs:\n  test:\n    "+line+"\n")
		violations, err := checkRepository(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(violations) != 1 {
			t.Fatalf("line %q violations = %+v, want one", line, violations)
		}
	}
}

func TestCheckGoFileCatchesIndirectFlakeSkipReason(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// The raw-text check alone misses this: the call expression's own source
	// ("t.Skip(reason)") contains no flake word, only the variable it names
	// does.
	writeFixture(t, root, "pkg/example_test.go", `package pkg
import "testing"
func TestExample(t *testing.T) {
	reason := "flaky"
	t.Skip(reason)
}
`)
	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %+v, want one", violations)
	}
	if !strings.Contains(violations[0].Message, "flake.Quarantine") {
		t.Fatalf("violations = %+v", violations)
	}
}

func TestCheckGoFileAllowsIndirectNonFlakeSkipReason(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFixture(t, root, "pkg/example_test.go", `package pkg
import "testing"
func TestExample(t *testing.T) {
	reason := "requires Linux"
	t.Skip(reason)
}
`)
	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %+v", violations)
	}
}

func TestCheckWorkflowAllowsNetworkRetryInsideRunBlock(t *testing.T) {
	t.Parallel()
	// A backoff retry around a network fetch (go mod download, curl) is
	// legitimate infra hardening against transient network flakiness — it
	// has nothing to do with masking a flaky *test*. The bare word "retry"
	// showing up in an echo message or a curl flag inside a run: block must
	// not trip the policy; only the shell retry-until-success idiom
	// (&&break / ||continue) should.
	root := t.TempDir()
	writeFixture(t, root, ".github/workflows/ci.yml", "jobs:\n  test:\n    steps:\n"+
		"      - run: |\n"+
		"          for attempt in 1 2 3; do\n"+
		"            if go mod download; then break; fi\n"+
		"            echo \"::warning::go mod download attempt $attempt/3 failed; retrying\"\n"+
		"          done\n"+
		"      - run: curl -sSfL --retry 3 --retry-delay 2 https://example.test\n")
	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %+v", violations)
	}
}

func TestCheckWorkflowRejectsRetryIdiom(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		`run: for n in 1 2 3; do make test && break; done`,
		`run: attempt=0; while [ $attempt -lt 3 ]; do make test || continue; break; done`,
	} {
		root := t.TempDir()
		writeFixture(t, root, ".github/workflows/ci.yml", "jobs:\n  test:\n    steps:\n      - "+line+"\n")
		violations, err := checkRepository(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(violations) == 0 {
			t.Fatalf("line %q: want a retry-idiom violation, got none", line)
		}
	}
}

func TestCheckWorkflowAllowsUnrelatedLoopWithBreak(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// A search-for-first-match loop is not a retry idiom: its break is
	// conditioned by an `if`, not chained directly off the command via
	// `&&`/`||`.
	writeFixture(t, root, ".github/workflows/ci.yml", "jobs:\n  test:\n    steps:\n"+
		"      - run: |\n"+
		"          for f in $(git diff --name-only); do\n"+
		"            if [ -s \"$f\" ]; then found=1; break; fi\n"+
		"          done\n")
	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %+v", violations)
	}
}

func TestCheckRepositoryAllowsNonFlakeSkipAndQuarantineHelper(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFixture(t, root, "pkg/example_test.go", `package pkg
import "testing"
func TestExample(t *testing.T) { t.Skip("requires Linux") }
`)
	writeFixture(t, root, quarantineHelper, `package flake
func quarantine(t interface{ Skip(string) }) { t.Skip("quarantined flaky test") }
`)
	writeFixture(t, root, ".github/workflows/ci.yml", `name: CI
jobs:
  test:
    steps:
      - run: go test ./...
`)
	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %+v", violations)
	}
}

func TestRepositoryCompliesWithFlakePolicy(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("repository flake-policy violations: %+v", violations)
	}
}

func writeFixture(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
