package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/worktree"
)

// TestComposeGitEnvCarriesCommitIdentity pins #4170: rebase-pr replays commits
// on a workspace it did not create, and git refuses to write one without a
// committer. Both branches of composeGitEnv must carry the identity — the
// credentialled path is the one rebase-pr actually takes.
func TestComposeGitEnvCarriesCommitIdentity(t *testing.T) {
	want := map[string]string{
		"GIT_AUTHOR_NAME":     worktree.BotGitUserName,
		"GIT_AUTHOR_EMAIL":    worktree.BotGitUserEmail,
		"GIT_COMMITTER_NAME":  worktree.BotGitUserName,
		"GIT_COMMITTER_EMAIL": worktree.BotGitUserEmail,
	}
	for _, tc := range []struct {
		name    string
		authEnv []string
	}{
		{name: "no credential"},
		{name: "with credential", authEnv: []string{
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=credential.helper",
			"GIT_CONFIG_VALUE_0=!f(){ :; }; f",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := composeGitEnv(t.TempDir(), tc.authEnv)
			got := map[string]string{}
			for _, entry := range env {
				name, value, _ := strings.Cut(entry, "=")
				if _, ok := want[name]; ok {
					got[name] = value
				}
			}
			for name, value := range want {
				if got[name] != value {
					t.Fatalf("%s = %q, want %q (env=%v)", name, got[name], value, env)
				}
			}
		})
	}
}

// TestComposeGitEnvLetsGitRebaseCommit is the end of the same argument, run
// against real git: the environment alone, with no user.email anywhere, has to
// be enough to replay a commit.
func TestComposeGitEnvLetsGitRebaseCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	env := append(composeGitEnv(dir, nil), "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "--initial-branch=main", ".")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-m", "base")
	run("checkout", "-b", "topic")
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("topic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "b.txt")
	run("commit", "-m", "topic")
	run("checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "c.txt")
	run("commit", "-m", "main moves")
	run("checkout", "topic")
	run("rebase", "main")
}
