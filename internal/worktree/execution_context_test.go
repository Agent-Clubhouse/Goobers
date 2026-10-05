package worktree

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/platform/proc"
)

func TestRunGitIdentityOverridesSharedManagerAndLocalCommands(t *testing.T) {
	calls := 0
	fallback := false
	manager := &Manager{gitEnv: func(context.Context, string) ([]string, error) {
		fallback = true
		return nil, errors.New("automation resolver must not run")
	}}
	ctx := WithGitExecution(t.Context(), GitExecution{Environment: func(_ context.Context, remote string) ([]string, error) {
		if remote != "https://example.test/repo" {
			return nil, errors.New("wrong scope")
		}
		return []string{"HUMAN=only"}, nil
	}, Prepare: func(ctx context.Context, _ []string) (context.Context, []string, func(), error) {
		calls++
		return ctx, []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=Human Identity", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}, func() {}, nil
	}})
	env, err := manager.executionGitEnvironment(ctx, "https://example.test/repo")
	if err != nil || len(env) != 1 || fallback {
		t.Fatalf("env=%v err=%v fallback=%v", env, err, fallback)
	}
	output, err := gitOutput(context.WithoutCancel(ctx), t.TempDir(), "config", "user.name")
	if errors.Is(err, proc.ErrQuiescenceUnobservable) {
		if calls != 1 {
			t.Fatal("command authority was not applied")
		}
		t.Log("host denies process inventory; strict Git execution correctly refused unobservable termination")
		return
	}
	if err != nil || strings.TrimSpace(output) != "Human Identity" || calls != 1 {
		t.Fatalf("output=%q err=%v prepare=%d", output, err, calls)
	}
	if _, err := manager.executionGitEnvironment(ctx, "https://other.test/repo"); err == nil {
		t.Fatal("unmatched remote used ambient identity")
	}
}
func TestRunGitIncompleteAuthorityRefuses(t *testing.T) {
	ctx := WithGitExecution(t.Context(), GitExecution{})
	if _, err := (&Manager{}).executionGitEnvironment(ctx, "remote"); err == nil {
		t.Fatal("missing authority")
	}
	if _, err := gitOutput(ctx, t.TempDir(), "version"); err == nil {
		t.Fatal("missing command authority")
	}
}
