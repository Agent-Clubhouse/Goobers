//go:build integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Run the actual workflow guard against a local repository. No GitHub API,
// signing credential, or remote is involved in this authorization check.
func TestIntegrationReleaseSourceAuthorization(t *testing.T) {
	testdep.Require(t, "bash", "git")
	workflow := loadReleaseAuthorizationWorkflow(t)
	script := workflow.Jobs["build"].Steps[1].Run
	repository := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repository
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(repository, "missing-global-config"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--initial-branch=main")
	git("config", "user.name", "Release authorization test")
	git("config", "user.email", "release-test@example.invalid")
	git("-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "first source")
	first := git("rev-parse", "HEAD")
	git("tag", "v0.3.3")
	git("-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "release source")
	source := git("rev-parse", "HEAD")
	git("tag", "v0.4.0")
	git("branch", "v0.4.0")
	git("-c", "tag.gpgsign=false", "tag", "-a", "v0.4.0-rc.1", "-m", "annotated candidate")
	annotation := git("rev-parse", "refs/tags/v0.4.0-rc.1")
	for _, tag := range []string{"v01.4.0", "v0.04.0", "v0.4.00", "v0.4.0-rc.01", "v0.4.0+build.1", "v0.4.0-01a"} {
		git("tag", tag)
	}
	for _, tc := range []struct {
		name, event, ref, expected, head string
		allowed                          bool
	}{
		{"stable push", "push", "refs/tags/v0.4.0", source, source, true},
		{"stable manual", "workflow_dispatch", "refs/tags/v0.4.0", source, source, true},
		{"annotated candidate push", "push", "refs/tags/v0.4.0-rc.1", annotation, source, true},
		{"candidate manual", "workflow_dispatch", "refs/tags/v0.4.0-rc.1", source, source, true},
		{"alphanumeric prerelease", "push", "refs/tags/v0.4.0-01a", source, source, true},
		{"branch manual", "workflow_dispatch", "refs/heads/main", source, source, false},
		{"tag named branch", "workflow_dispatch", "refs/heads/v0.4.0", source, source, false},
		{"unexpected event", "pull_request", "refs/tags/v0.4.0", source, source, false},
		{"missing tag", "push", "refs/tags/v0.9.0", source, source, false},
		{"tag source mismatch", "push", "refs/tags/v0.3.3", source, source, false},
		{"event source mismatch", "push", "refs/tags/v0.4.0", first, source, false},
		{"checkout mismatch", "push", "refs/tags/v0.4.0", source, first, false},
		{"abbreviated source", "push", "refs/tags/v0.4.0", source[:12], source, false},
		{"source expression", "push", "refs/tags/v0.4.0", "HEAD", source, false},
		{"leading major zero", "push", "refs/tags/v01.4.0", source, source, false},
		{"leading minor zero", "push", "refs/tags/v0.04.0", source, source, false},
		{"leading patch zero", "push", "refs/tags/v0.4.00", source, source, false},
		{"leading prerelease zero", "push", "refs/tags/v0.4.0-rc.01", source, source, false},
		{"empty prerelease component", "push", "refs/tags/v0.4.0-rc..1", source, source, false},
		{"unsupported build suffix", "push", "refs/tags/v0.4.0+build.1", source, source, false},
		{"untrusted shell text", "push", "refs/tags/v0.4.0;exit 0", source, source, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git("checkout", "--detach", tc.head)
			cmd := exec.Command("bash", "--noprofile", "--norc", "-c", script)
			cmd.Dir = repository
			outputFile := filepath.Join(t.TempDir(), "github-output")
			cmd.Env = append(os.Environ(), "RELEASE_EVENT="+tc.event, "RELEASE_REF="+tc.ref, "EXPECTED_SOURCE="+tc.expected, "GITHUB_OUTPUT="+outputFile)
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.allowed {
				t.Fatalf("authorization allowed=%v; want %v: %v\n%s", err == nil, tc.allowed, err, out)
			}
			if tc.allowed {
				identity, err := os.ReadFile(outputFile)
				if err != nil || string(identity) != "commit="+source+"\n" {
					t.Fatalf("authorized identity = %q: %v; want peeled source %s", identity, err, source)
				}
			}
		})
	}
}
