package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
)

func TestExternalTelemetryOutboxUsesResolvedCredentialScrubber(t *testing.T) {
	t.Chdir(t.TempDir())
	setPodExternalTelemetryEnv(t, defaultExternalTelemetryFixture())
	t.Setenv(dispatcher.EnvStageOutbox, `["report.txt"]`)
	uploaded := make(chan string, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		uploaded <- string(data)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	t.Setenv(dispatcher.EnvBlobEndpoint, server.URL)
	out := runStage(context.Background(), io.Discard, io.Discard)
	token := "minted-for-" + string(capability.TelemetryRead)
	if out.Result.Status != apiv1.ResultSuccess || out.OutboxScrubber == nil {
		t.Fatalf("bad fixture: %+v", out)
	}
	if err := os.WriteFile("report.txt", []byte("diagnostic payload: "+token), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := publishPodOutbox(t.Context(), out.OutboxScrubber); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case got := <-uploaded:
			if strings.Contains(got, token) {
				t.Fatalf("credential-aware scrubber leaked token in %q", got)
			}
		default:
			return
		}
	}
}
