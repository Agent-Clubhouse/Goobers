package worktree

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestRetryBotIdentityConfigRetriesLockContention(t *testing.T) {
	calls := 0
	err := retryBotIdentityConfig(context.Background(), func() error {
		calls++
		if calls < 3 {
			return configLockContentionError()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryBotIdentityConfig returned %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want 3", calls)
	}
}

func TestRetryBotIdentityConfigIsBounded(t *testing.T) {
	calls := 0
	err := retryBotIdentityConfig(context.Background(), func() error {
		calls++
		return configLockContentionError()
	})
	if err == nil {
		t.Fatal("retryBotIdentityConfig returned nil, want lock error")
	}
	if calls != botIdentityRetryAttempts {
		t.Fatalf("op called %d times, want %d", calls, botIdentityRetryAttempts)
	}
}

func TestRetryBotIdentityConfigDoesNotRetryOtherFailures(t *testing.T) {
	calls := 0
	want := &gitCommandError{output: []byte("error: could not lock config file /repo.git/config: Permission denied")}
	err := retryBotIdentityConfig(context.Background(), func() error {
		calls++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("retryBotIdentityConfig returned %v, want %v", err, want)
	}
	if calls != 1 {
		t.Fatalf("op called %d times, want 1", calls)
	}
}

func TestRetryBotIdentityConfigHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retryBotIdentityConfig(ctx, func() error {
		calls++
		cancel()
		return configLockContentionError()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("retryBotIdentityConfig returned %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("op called %d times, want 1", calls)
	}
}

func TestIsGitConfigLockContentionRequiresTypedGitError(t *testing.T) {
	if isGitConfigLockContention(fmt.Errorf("could not lock config file config: File exists")) {
		t.Fatal("isGitConfigLockContention matched an untyped error")
	}
	if !isGitConfigLockContention(fmt.Errorf("set identity: %w", configLockContentionError())) {
		t.Fatal("isGitConfigLockContention did not match a wrapped git config lock error")
	}
}

func configLockContentionError() error {
	return &gitCommandError{
		exitCode: 255,
		output:   []byte("error: could not lock config file /repo.git/config: File exists"),
	}
}
