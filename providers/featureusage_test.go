package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/diagnostics/featureusage"
	"github.com/goobers/goobers/internal/journal"
)

type usageRecorder struct{ events []journal.Event }

func (r *usageRecorder) Append(event journal.Event) error {
	r.events = append(r.events, event)
	return nil
}
func TestProviderHTTPUsageCountsActualRequestsWithoutPrivatePayloads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOOBERS_TELEMETRY_DIR", dir)
	featureusage.BeginProviderWindow(dir)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	github := NewGitHubProvider("private-token")
	gitea := NewGiteaProvider(server.URL, "private-token")
	ado := NewADOProvider("private-org", "private-project", "private-token")
	// Construction/configuration above must not record use.
	empty := &usageRecorder{}
	featureusage.CollectProviderWindow(dir, empty, "stage")
	if len(empty.events) != 0 {
		t.Fatal("configuration counted as use")
	}
	for _, call := range []func() error{
		func() error {
			return github.do(context.Background(), http.MethodGet, server.URL+"/private-repo", nil, nil)
		},
		func() error {
			return gitea.do(context.Background(), http.MethodGet, server.URL+"/private-repo", nil, nil)
		},
		func() error {
			return ado.do(context.Background(), http.MethodGet, server.URL+"/private-project", nil, nil)
		},
	} {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}
	rec := &usageRecorder{}
	featureusage.CollectProviderWindow(dir, rec, "stage")
	if requests != 3 || len(rec.events) != 3 {
		t.Fatalf("requests=%d events=%v", requests, rec.events)
	}
	for _, event := range rec.events {
		if event.Runner["count"] != int64(1) {
			t.Fatal(event)
		}
	}
	data, _ := json.Marshal(rec.events)
	if strings.Contains(string(data), "private-") {
		t.Fatal("private provider fields leaked")
	}
	featureusage.BeginProviderWindow(dir)
	reset := &usageRecorder{}
	featureusage.CollectProviderWindow(dir, reset, "stage")
	if len(reset.events) != 0 {
		t.Fatal("previous stage counted again")
	}
}
