package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestChildSnapshotPolicyExcludesKnownCredentialAndInstancePaths(t *testing.T) {
	workspace := t.TempDir()
	inside := filepath.Join(workspace, "private-provider-token")
	if err := os.WriteFile(inside, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &instance.Config{Webhook: instance.WebhookConfig{Secret: instance.TokenRef{File: inside}}, Credentials: []instance.CredentialGrant{{Token: instance.TokenRef{File: filepath.Join(t.TempDir(), "outside-token")}}}}
	policy, err := childSnapshotPolicy(workspace, filepath.Join(workspace, "factory-state"), cfg)
	if err != nil || !slices.Equal(policy.ExcludedPaths, []string{"factory-state", "private-provider-token"}) {
		t.Fatal(policy, err)
	}
	if _, err := childSnapshotPolicy(workspace, workspace, cfg); err == nil {
		t.Fatal("instance storage itself accepted as a source workspace")
	}
	cfg.Webhook.Secret.File = filepath.Join(workspace, "unsupported*pattern")
	if _, err := childSnapshotPolicy(workspace, t.TempDir(), cfg); err == nil {
		t.Fatal("unrepresentable guarded path silently omitted")
	}
}
