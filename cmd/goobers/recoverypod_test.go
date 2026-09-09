package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/dispatcher"
)

func TestPodRecoveryNonWritableWorkspaceNeedsNoCustody(t *testing.T) {
	for _, mode := range []string{"scratch", "repo-readonly"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(dispatcher.EnvStageWorkspace, mode)
			t.Setenv(dispatcher.EnvDaemonAPI, "")
			if err := publishPodRecovery(t.Context(), "missing-workspace"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPodRecoveryMissingClaimPreservesSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/claims/list" || r.Header.Get("Authorization") != "Bearer parent-token" {
			t.Error("recovery did not use the parent claim credential")
		}
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	defer server.Close()
	t.Setenv(dispatcher.EnvStageWorkspace, "repo")
	t.Setenv(dispatcher.EnvRunID, "pod-recovery")
	t.Setenv(dispatcher.EnvGaggle, "web")
	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvPodToken, "parent-token")
	root := t.TempDir()
	path := filepath.Join(root, "implementation")
	if err := os.WriteFile(path, []byte("retain me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishPodRecovery(t.Context(), root); err == nil {
		t.Fatal("missing claim acknowledged recovery")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "retain me" {
		t.Fatalf("unacknowledged source changed: %q %v", data, err)
	}
}
