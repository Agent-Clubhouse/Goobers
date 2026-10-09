package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/dispatcher"
)

func TestChildPodEnvironmentDropsInheritedAuthentication(t *testing.T) {
	before := os.Environ()
	t.Cleanup(func() {
		os.Clearenv()
		for _, entry := range before {
			key, value, _ := strings.Cut(entry, "=")
			_ = os.Setenv(key, value)
		}
	})
	for key, value := range map[string]string{"UNDECLARED_PROVIDER_TOKEN": "secret", "AZURE_CONFIG_DIR": "/image/auth", "DECLARED_TEST_VALUE": "kept", dispatcher.EnvPodToken: "owned-pod-token"} {
		if err := os.Setenv(key, value); err != nil {
			t.Fatal(err)
		}
	}
	allowed, _ := json.Marshal([]string{"DECLARED_TEST_VALUE"})
	if err := os.Setenv(dispatcher.EnvStageEnvAllow, string(allowed)); err != nil {
		t.Fatal(err)
	}
	if err := cleanChildPodEnvironment(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("UNDECLARED_PROVIDER_TOKEN") != "" || os.Getenv("AZURE_CONFIG_DIR") != "" {
		t.Fatal("image auth survived child entrypoint")
	}
	if os.Getenv("DECLARED_TEST_VALUE") != "kept" || os.Getenv(dispatcher.EnvPodToken) != "owned-pod-token" {
		t.Fatal("explicit entrypoint inputs were dropped")
	}
	if os.Getenv("HOME") != dispatcher.LinuxHomePath || os.Getenv("GIT_CONFIG_GLOBAL") != "/dev/null" {
		t.Fatal("inherited HOME/git configuration survived")
	}
}

func TestChildPodCheckoutRequiresTrustedMaterializedContext(t *testing.T) {
	ctx := context.WithValue(t.Context(), isolatedChildKey{}, true)
	t.Setenv(dispatcher.EnvStageWorkspace, "repo")
	if err := checkoutRepoWorkspace(ctx, t.TempDir(), io.Discard, nil, ""); err != nil {
		t.Fatal("materialized child attempted remote clone", err)
	}
	if credentials, _, err := podCheckoutCredentials(ctx, nil, ""); err != nil || len(credentials) != 0 {
		t.Fatal("child minted checkout credential", err)
	}
	if os.Getpid() != 1 && childpod.VerifyEntrypoint() == nil {
		t.Fatal("ordinary local process accepted child supervisor boundary")
	}
}
