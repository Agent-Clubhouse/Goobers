package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// TestStageADOCredentialSourceReportsExpiryFromTheDeliveredVariable pins the
// stage half of #5905: the ADO stage provider reads the expiry the daemon
// delivered as GOOBERS_CREDENTIAL_EXPIRES_<capability>, so a 401 after it
// says the credential expired and a 401 before it says it was revoked or
// lacks access. Without the variable (a standalone invocation), or with an
// unreadable one, the error keeps the combined wording.
func TestStageADOCredentialSourceReportsExpiryFromTheDeliveredVariable(t *testing.T) {
	t.Parallel()
	const credential = "ado-expiry-token-canary"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("TF400813: not authorized"))
	}))
	t.Cleanup(server.Close) // not defer: the parallel subtests outlive this function body

	for _, tc := range []struct {
		name   string
		expiry string
		want   string
	}{
		{name: "expired", expiry: "2020-01-02T03:04:05Z", want: "(expired at 2020-01-02T03:04:05Z)"},
		{name: "not yet expired", expiry: "2099-01-02T03:04:05Z", want: "(revoked or without access to this resource; it does not expire until 2099-01-02T03:04:05Z)"},
		{name: "no expiry delivered", want: "(expired, revoked, or without access to this resource)"},
		{name: "unreadable expiry", expiry: "soon", want: "(expired, revoked, or without access to this resource)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := stageEnvFor(map[string]string{
				executor.RepoAuthSchemeEnvVar:                                  "bearer",
				capability.CredentialExpiryEnvVar(string(capability.RepoPush)): tc.expiry,
			})
			source, err := stageADOCredentialSourceFrom(env, capability.RepoPush, credential)
			if err != nil {
				t.Fatalf("stageADOCredentialSource: %v", err)
			}
			provider, err := buildADOProviderForStage(
				providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "example-org", Project: "example-project", Name: "example-repo"},
				source,
			)
			if err != nil {
				t.Fatalf("buildADOProviderForStage: %v", err)
			}
			provider.BaseURL = server.URL
			_, err = provider.GetWorkItem(context.Background(), providers.RepositoryRef{Project: "example-project"}, "42")
			if !errors.Is(err, providers.ErrADODeliveredCredentialRejected) {
				t.Fatalf("GetWorkItem error = %v, want ErrADODeliveredCredentialRejected", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q, want it to contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), credential) {
				t.Fatalf("error leaks the credential: %q", err)
			}
		})
	}
}
