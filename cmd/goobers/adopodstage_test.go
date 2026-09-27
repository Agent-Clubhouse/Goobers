package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
)

// TestADOStageInPodRunsOnTheMintedCredentialAlone is ADO-N18's pod proof: an
// Azure DevOps provider command runs with nothing but what a stage pod has —
// the routed repository, its declared capability, and the credential the
// daemon's credential plane minted for it with the stated scheme. There is no
// instance root and no instance.yaml, so the command cannot fall back to a
// configured repos[].auth; before ADO-N18 the ADO factory required one.
//
// The credential crosses the same seams a pod uses: the plane answers
// resolveStageCredentialsWithScheme over HTTP, stageCredentialEnv renders the
// stage environment from that answer, and report-pr-status builds its provider
// from GOOBERS_CRED_GITHUB_PR_WRITE plus GOOBERS_REPO_AUTH_SCHEME.
func TestADOStageInPodRunsOnTheMintedCredentialAlone(t *testing.T) {
	const minted = "minted-pr-write-token"

	var mu sync.Mutex
	var sent []string
	ado := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.Header.Get("Authorization")+"|"+r.Header.Get("X-VSS-ForceMsaPassThrough"))
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pullrequests/77/iterations"):
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]int{{"id": 1}, {"id": 3}}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pullrequests/77/iterations/3/statuses"):
			_ = json.NewEncoder(w).Encode(map[string]int{"id": 11})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ado.Close)

	plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apicontract.CredentialResolvePath {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Capabilities []string `json:"capabilities"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &request); err != nil || !slices.Equal(request.Capabilities, []string{"github:pr:write"}) {
			t.Errorf("credential resolve request = %s, want exactly the declared github:pr:write", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"credentials":    []map[string]string{{"capability": "github:pr:write", "value": minted}},
			"repoAuthScheme": "bearer",
		})
	}))
	t.Cleanup(plane.Close)

	// A stage pod: no instance root, and a working directory with no
	// instance.yaml anywhere in it.
	t.Setenv(executor.InstanceRootEnvVar, "")
	workDir := t.TempDir()
	t.Chdir(workDir)
	if _, err := os.Stat(instance.NewLayout(".").ConfigFile()); !os.IsNotExist(err) {
		t.Fatalf("fixture has an instance config (stat error %v); a pod has none", err)
	}
	for _, name := range everyCredentialedCapability() {
		t.Setenv(executor.CredentialEnvVar(name), "")
	}
	t.Setenv(executor.RepoAuthSchemeEnvVar, "")
	t.Setenv(dispatcher.EnvDaemonAPI, plane.URL)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvRunID, "run-ado-pod")
	t.Setenv(dispatcher.EnvStage, "report-status")
	t.Setenv(dispatcher.EnvStageCapabilities, `["github:pr:write"]`)
	t.Setenv(executor.RepoProviderEnvVar, "ado")
	t.Setenv(executor.RepoOwnerEnvVar, "example-org")
	t.Setenv(executor.RepoProjectEnvVar, "example-project")
	t.Setenv(executor.RepoNameEnvVar, "example-repo")
	t.Setenv(executor.InputEnvVar("prNumber"), "77")

	creds, scheme, err := resolveStageCredentialsWithScheme(context.Background())
	if err != nil {
		t.Fatalf("resolve stage credentials: %v", err)
	}
	for _, entry := range stageCredentialEnv(creds, scheme) {
		name, value, _ := strings.Cut(entry, "=")
		t.Setenv(name, value)
	}
	pointADOStageProviderAt(t, ado)

	code, stdout, stderr := runArgs(t, "report-pr-status")
	if code != 0 {
		t.Fatalf("report-pr-status in a pod: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 {
		t.Fatalf("ADO requests = %d (%q), want the iteration read and the status post", len(sent), sent)
	}
	for _, got := range sent {
		if got != "Bearer "+minted+"|true" {
			t.Fatalf("ADO request authenticated as %q, want the minted bearer with the MSA passthrough header", got)
		}
	}
}
