package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/diagnostics/featureusage"
	"github.com/goobers/goobers/providers"
)

func TestFeatureProviderProcessHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-1] != "feature-provider-helper" {
		return
	}
	provider := providers.NewGitHubProvider("synthetic-private-token")
	provider.BaseURL = os.Args[len(os.Args)-2]
	if _, err := provider.RepositorySizeKB(context.Background(), providers.RepositoryRef{Owner: "synthetic-private-owner", Name: "repo"}); err != nil {
		t.Fatal(err)
	}
}
func TestShellExecutorCollectsRealProviderProcessUsage(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"size":1}`))
	}))
	defer server.Close()
	executor, recorder := newPortableTestExecutor(t, nil)
	env := baseEnvelope(t)
	result, err := executor.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{os.Args[0], "-test.run=^TestFeatureProviderProcessHelper$", "--", server.URL, "feature-provider-helper"}})
	if err != nil || result.Status != apiv1.ResultSuccess {
		t.Fatalf("%+v %v", result, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests=%d", requests.Load())
	}
	found := 0
	for _, event := range recorder.events {
		if event.Runner["kind"] == featureusage.Kind {
			found++
			if event.Runner["featureId"] != "provider.github" || event.Runner["count"] != int64(1) {
				t.Fatal(event)
			}
		}
	}
	if found != 1 {
		t.Fatalf("provider process evidence missing: %+v", recorder.events)
	}
}
