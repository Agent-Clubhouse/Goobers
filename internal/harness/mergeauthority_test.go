package harness

import (
	"slices"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
)

func TestAgenticMergeCLIUsesTrustedAuthorityAndPinnedConfiguration(t *testing.T) {
	ctx := executor.WithJournalPlane(t.Context(), executor.JournalPlane{Endpoint: "http://daemon", Token: "scoped-read-token"})
	ctx = executor.WithConfigDirectory(ctx, "/immutable/config")
	req := RunRequest{Envelope: apiv1.InvocationEnvelope{InstanceID: "instance-1", RunID: "run-1", TaskID: "run-1:merge", Gaggle: "example", WorkflowID: "implementation", ConfigGeneration: "sha256:pinned", Capabilities: []string{"github:pr:merge"}}}
	t.Setenv("GOOBERS_JOURNAL_ENDPOINT", "http://stale-worker")
	env, err := buildCredentialEnv(ctx, credentialEnvConfig{extraEnvAllowlist: []string{"GOOBERS_JOURNAL_ENDPOINT"}}, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"GOOBERS_RUN_ID=run-1", "GOOBERS_INSTANCE_ID=instance-1", "GOOBERS_TASK=merge", "GOOBERS_JOURNAL_ENDPOINT=http://daemon", "GOOBERS_JOURNAL_TOKEN=scoped-read-token", "GOOBERS_CONFIG_DIRECTORY=/immutable/config", "GOOBERS_CONFIG_GENERATION=sha256:pinned"} {
		if !slices.Contains(env, expected) {
			t.Errorf("missing %s", expected)
		}
	}
	if slices.Contains(env, "GOOBERS_JOURNAL_ENDPOINT=http://stale-worker") {
		t.Fatal("ambient routing overrode admitted authority")
	}
	shell := codexMergeAuthorityShellEnvironment(ctx, req, codexShellEnvironment(append(env, "GH_TOKEN=provider-secret"), []string{"GH_TOKEN"}), "/instance")
	if shell["GOOBERS_JOURNAL_TOKEN"] != "scoped-read-token" || shell["GOOBERS_TASK"] != "merge" {
		t.Fatal("Codex shell lost authority")
	}
	if shell["GH_TOKEN"] != "" {
		t.Fatal("provider secret restriction widened")
	}
	req.Envelope.Capabilities = nil
	env, err = buildCredentialEnv(ctx, credentialEnvConfig{}, req)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(env, "GOOBERS_JOURNAL_TOKEN=scoped-read-token") {
		t.Fatal("authority leaked to non-merge invocation")
	}
}
