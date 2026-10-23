package runtimewiring_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/runtimewiring"
)

func TestMergeAuthorityUsesRemoteCurrentGrantAndPinnedDirectory(t *testing.T) {
	for _, cap := range []capability.Capability{capability.GitHubPRMerge, capability.ADOPRComplete} {
		t.Run(string(cap), func(t *testing.T) {
			var allowed atomic.Bool
			var requests atomic.Int64
			reached := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var request journalclient.MergeAuthorityRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.RunID != "run" || request.Gaggle != "example" || request.Stage != "land" || request.Capability != string(cap) {
					t.Errorf("wrong authority scope: %+v", request)
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(journalclient.MergeAuthorityResponse{Allowed: allowed.Load()}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			layout := instance.NewLayout(t.TempDir()).WithConfigDir(t.TempDir())
			fence := runtimewiring.WithCurrentMergeAuthority(layout, func(ctx context.Context, _ apiv1.InvocationEnvelope) (context.Context, context.CancelFunc, error) {
				reached++
				if got := executor.ConfigDirectoryFromContext(ctx); got != layout.ConfigDir() {
					t.Errorf("config directory %q, want %q", got, layout.ConfigDir())
				}
				return ctx, func() {}, nil
			})
			ctx := executor.WithJournalPlane(t.Context(), executor.JournalPlane{Endpoint: server.URL})
			env := apiv1.InvocationEnvelope{RunID: "run", TaskID: "run:land", Gaggle: "example", ConfigGeneration: "pinned", Capabilities: []string{string(cap)}}
			_, stop, err := fence(ctx, env)
			stop()
			if err == nil || reached != 0 || requests.Load() != 1 {
				t.Fatalf("revocation bypass: err=%v reached=%d requests=%d", err, reached, requests.Load())
			}
			allowed.Store(true)
			_, stop, err = fence(ctx, env)
			stop()
			if err != nil || reached != 1 || requests.Load() != 2 {
				t.Fatalf("current grant rejected: err=%v reached=%d requests=%d", err, reached, requests.Load())
			}
		})
	}
}
