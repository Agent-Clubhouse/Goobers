//go:build integration

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Compile tiny real Go binaries, including a foreign target, and inspect only
// their metadata. These are fixtures, not candidate executions or publisher proof.
func TestIntegrationReleaseSourceAndBinaryProvenance(t *testing.T) {
	testdep.Require(t, "git", "go")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOENV", "off")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOFLAGS", "")
	for _, scenario := range []string{"clean", "ignored output", "untracked baseline", "tracked edit", "staged edit", "missing VCS"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			write := func(path, data string) {
				t.Helper()
				path = filepath.Join(repo, path)
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write("go.mod", "module github.com/goobers/goobers\n\ngo 1.26.0\n")
			write(".gitignore", "ignored/\n")
			program := "package main\nfunc main() { panic(\"metadata fixture must never execute\") }\n"
			write("cmd/goobers/main.go", program)
			write("cmd/operator/main.go", program)
			t.Chdir(repo)
			git := func(args ...string) string {
				t.Helper()
				out, err := testgit.Command(args...).CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git("init", "--initial-branch=main")
			git("config", "user.name", "Source provenance fixture")
			git("config", "user.email", "fixture@example.invalid")
			git("add", ".")
			git("-c", "commit.gpgsign=false", "commit", "-m", "fixture")
			source := git("rev-parse", "HEAD")
			switch scenario {
			case "ignored output":
				write("ignored/output", "generated output\n")
			case "untracked baseline":
				write(".release-baseline/snapshot.json", "{}\n")
			case "tracked edit", "staged edit":
				write("cmd/goobers/main.go", program+"// changed tracked input\n")
				if scenario == "staged edit" {
					git("add", "cmd/goobers/main.go")
				}
			case "missing VCS":
				t.Setenv("GOFLAGS", "-buildvcs=false")
			}
			clean := scenario == "clean" || scenario == "ignored output" || scenario == "missing VCS"
			if err := verifyReleaseSource(source); (err == nil) != clean {
				t.Fatalf("source guard clean=%v: %v", clean, err)
			}
			if err := verifyReleaseSource(strings.Repeat("a", 40)); err == nil {
				t.Fatal("accepted wrong checkout source")
			}
			if !clean {
				args := []string{"-version=v1.2.3", "-commit=" + source[:12], "-source-commit=" + source, "-first-feature-snapshot", "-output=" + filepath.Join(root, "refused")}
				if err := run(args, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "release source has") {
					t.Fatalf("packager must stop before building dirty source: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root, "refused")); !os.IsNotExist(err) {
					t.Fatal("dirty source created output before refusal")
				}
			}
			targets := []Target{{OS: "linux", Arch: "arm64"}}
			if scenario == "clean" {
				// Exercise ELF, Mach-O and PE metadata readers without executing any.
				targets = append(targets, Target{OS: "darwin", Arch: "arm64"}, Target{OS: "windows", Arch: "amd64"})
			}
			for _, target := range targets {
				for _, pkg := range []string{"./cmd/goobers", "./cmd/operator"} {
					binary := filepath.Join(root, filepath.Base(pkg)+"-"+target.OS)
					if out, err := buildReleaseBinary(target, "", binary, pkg); err != nil {
						t.Fatalf("fixture build: %v\n%s", err, out)
					}
					allowed := clean && scenario != "missing VCS"
					if err := verifyReleaseBinary(binary, source, pkg, target); (err == nil) != allowed {
						t.Fatalf("compiled binary allowed=%v: %v", allowed, err)
					}
				}
			}
		})
	}
}
