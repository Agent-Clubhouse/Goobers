package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestLocalAgenticExecutorRefusesConfigCredentialFiles(t *testing.T) {
	for _, posture := range []instance.SandboxPosture{instance.SandboxDisabled, instance.SandboxEnforced} {
		t.Run(string(posture), func(t *testing.T) {
			keyPath := filepath.Join(t.TempDir(), "credential.pem")
			const secret = "fixture-credential-content"
			if err := os.WriteFile(keyPath, []byte(secret), 0o600); err != nil {
				t.Fatal(err)
			}
			original := newAgenticAdapter
			t.Cleanup(func() { newAgenticAdapter = original })
			started := false
			newAgenticAdapter = func(string, map[string]string) harness.Adapter {
				return &harness.FakeAdapter{Act: func(_ context.Context, _ harness.RunRequest) error {
					started = true
					_, err := os.ReadFile(keyPath)
					return err
				}}
			}
			scrubber := journal.NewRegistryScrubber()
			cfg, _, err := buildRunnerConfig(runnerCompositionInput{
				Layout:               instance.NewLayout(t.TempDir()),
				Config:               &instance.Config{Webhook: instance.WebhookConfig{Secret: instance.TokenRef{File: keyPath}}},
				Goobers:              map[string]apiv1.GooberSpec{"coder": {}},
				InstructionsByGoober: map[string]string{"coder": "read the credential file"},
				SharedRegistry:       scrubber, SandboxPosture: posture,
			})
			if err != nil {
				t.Fatal(err)
			}
			agent, err := cfg.NewAgentic("coder", runnerWiringHarnessRecorder{dir: t.TempDir()}, scrubber)
			if !errors.Is(err, harness.ErrGuardedCredentialFiles) || agent != nil {
				t.Fatalf("NewAgentic = %v, %v; want credential refusal", agent, err)
			}
			if started {
				t.Fatal("agent read guarded file")
			}
			if strings.Contains(err.Error(), keyPath) || strings.Contains(err.Error(), secret) {
				t.Fatal("refusal disclosed credential path or content")
			}
		})
	}
}

func TestLocalAgenticExecutorAcceptsNonFileCredentialRefs(t *testing.T) {
	for name, ref := range map[string]instance.TokenRef{
		"none": {}, "environment": {Env: "EXAMPLE_TOKEN"},
		"keychain": {Keychain: "example-token"}, "store": {Store: "vault/token"},
	} {
		t.Run(name, func(t *testing.T) {
			scrubber := journal.NewRegistryScrubber()
			cfg, _, err := buildRunnerConfig(runnerCompositionInput{
				Layout:  instance.NewLayout(t.TempDir()),
				Config:  &instance.Config{Webhook: instance.WebhookConfig{Secret: ref}},
				Goobers: map[string]apiv1.GooberSpec{"coder": {}}, SharedRegistry: scrubber,
				InstructionsByGoober: map[string]string{"coder": "instructions"},
			})
			if err != nil {
				t.Fatal(err)
			}
			agent, err := cfg.NewAgentic("coder", runnerWiringHarnessRecorder{dir: t.TempDir()}, scrubber)
			if err != nil || agent == nil {
				t.Fatalf("NewAgentic = %v, %v", agent, err)
			}
		})
	}
}
