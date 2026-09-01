package main

import (
	"os/exec"
	"strings"
	"testing"
)

// gitConfigSlots reads the GIT_CONFIG_* environment a command was built with
// back into the key/value pairs git itself would apply.
func gitConfigSlots(t *testing.T, cmd *exec.Cmd) map[string]string {
	t.Helper()
	values := map[string]string{}
	keys := map[string]string{}
	vals := map[string]string{}
	count := ""
	for _, entry := range cmd.Env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch {
		case name == "GIT_CONFIG_COUNT":
			count = value
		case strings.HasPrefix(name, "GIT_CONFIG_KEY_"):
			keys[strings.TrimPrefix(name, "GIT_CONFIG_KEY_")] = value
		case strings.HasPrefix(name, "GIT_CONFIG_VALUE_"):
			vals[strings.TrimPrefix(name, "GIT_CONFIG_VALUE_")] = value
		}
	}
	if count == "" {
		t.Fatalf("command env declares no GIT_CONFIG_COUNT, so git applies none of %v", keys)
	}
	for slot, key := range keys {
		values[strings.ToLower(key)] = vals[slot]
	}
	return values
}

// A stage pod's /workspace is not owned by the container user, so every git
// call needs the safe.directory exemption, and a call against origin needs a
// credential too. Both travel in GIT_CONFIG_*, and assigning either alone
// erases the other by restating GIT_CONFIG_COUNT.
//
// gitPushBranch learned this in-pod and was fixed in isolation; every other
// authenticated git call in the remediation path kept `cmd.Env =
// gitAuthEnv(token)` and therefore kept losing safe.directory. Live on
// 2026-09-01, pr-remediation run 247dd57b9530db3ee3879365de4ef70c died in
// gather-pr-context with `fatal: detected dubious ownership in repository at
// '/workspace'` on the fetch.
func TestWorkspaceGitCommandsCarryBothConfigSlots(t *testing.T) {
	const dir = "/workspace"
	const token = "test-token"

	t.Run("an authenticated command carries the credential and the exemption", func(t *testing.T) {
		cmd := workspaceGitAuthCommand(dir, token, "fetch", "https://example.test/repo", "refs/heads/topic")
		slots := gitConfigSlots(t, cmd)
		if got := slots["safe.directory"]; got == "" {
			t.Fatalf("safe.directory absent from %v — git refuses a workspace it does not own", slots)
		}
		auth, ok := slots["http.extraheader"]
		if !ok {
			t.Fatalf("http.extraheader absent from %v — the fetch would be unauthenticated", slots)
		}
		if !strings.HasPrefix(auth, "AUTHORIZATION: basic ") {
			t.Fatalf("http.extraheader = %q, want a basic credential", auth)
		}
		if cmd.Dir != dir {
			t.Fatalf("cmd.Dir = %q, want %q", cmd.Dir, dir)
		}
	})

	t.Run("a prebuilt credential environment keeps the exemption too", func(t *testing.T) {
		cmd := workspaceGitAuthEnvCommand(dir, gitAuthEnv(token), "push", "https://example.test/repo", "topic:topic")
		slots := gitConfigSlots(t, cmd)
		if slots["safe.directory"] == "" || slots["http.extraheader"] == "" {
			t.Fatalf("slots = %v, want both safe.directory and http.extraheader", slots)
		}
	})

	t.Run("an unauthenticated command carries the exemption", func(t *testing.T) {
		cmd := workspaceGitCommand(dir, "rev-parse", "FETCH_HEAD")
		slots := gitConfigSlots(t, cmd)
		if got := slots["safe.directory"]; got != dir {
			t.Fatalf("safe.directory = %q, want %q", got, dir)
		}
	})

	t.Run("gitAuthEnv alone is exactly what must never be assigned directly", func(t *testing.T) {
		bare := &exec.Cmd{Env: gitAuthEnv(token)}
		slots := gitConfigSlots(t, bare)
		if _, ok := slots["safe.directory"]; ok {
			t.Fatal("gitAuthEnv now carries safe.directory; the composition helpers are redundant and this test is stale")
		}
	})
}
