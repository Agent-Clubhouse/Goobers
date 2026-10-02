package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestExecutorRefusesGuardedCredentialFilesBeforeAgentStarts(t *testing.T) {
	for _, enforced := range []bool{false, true} {
		name := "default"
		if enforced {
			name = "enforced-write-sandbox"
		}
		t.Run(name, func(t *testing.T) {
			keyPath := filepath.Join(t.TempDir(), "private-key.pem")
			const secret = "fixture-private-key-content"
			if err := os.WriteFile(keyPath, []byte(secret), 0o600); err != nil {
				t.Fatal(err)
			}
			run := newSandboxTestRun(t)
			started := false
			adapter := &FakeAdapter{Act: func(_ context.Context, _ RunRequest) error {
				started = true
				_, err := os.ReadFile(keyPath)
				return err
			}}
			opts := []Option{WithGuardedCredentialPaths([]string{keyPath})}
			if enforced {
				opts = append(opts, WithSandboxEnforcement())
			}
			exec, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), run, run,
				NewContextResolver(run, t.TempDir()), journal.NewPatternScrubber(), "read the credential file", opts...)
			if !errors.Is(err, ErrGuardedCredentialFiles) || exec != nil {
				t.Fatalf("NewExecutor = %v, %v; want refusal", exec, err)
			}
			if started {
				t.Fatal("agent started despite guarded credential files")
			}
			if strings.Contains(err.Error(), keyPath) || strings.Contains(err.Error(), secret) {
				t.Fatal("refusal disclosed credential path or contents")
			}
			events, readErr := os.ReadFile(filepath.Join(run.Dir(), "events.jsonl"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if strings.Contains(string(events), keyPath) || strings.Contains(string(events), secret) {
				t.Fatal("journal disclosed credential path or contents")
			}
		})
	}
}

func TestExecutorWithoutGuardedCredentialFilesCanBeConstructed(t *testing.T) {
	rec := &minimalRecorder{}
	exec, err := NewExecutor(&FakeAdapter{}, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec,
		journal.NewPatternScrubber(), "instructions", WithGuardedCredentialPaths(nil))
	if err != nil || exec == nil {
		t.Fatalf("NewExecutor = %v, %v", exec, err)
	}
}
