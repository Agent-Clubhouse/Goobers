package workbenchservice

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
)

func TestHumanPRSelectionUsesExactInteractiveSourceAndNativeIdentity(t *testing.T) {
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte("hidden-description-canary"))
	reads := 0
	fork := false
	transport := roundTrip(func(r *http.Request) (*http.Response, error) {
		reads++
		if r.Method != http.MethodGet || r.URL.Host != "api.github.com" || r.URL.Path != "/repos/acme/code/pulls/42" || r.Header.Get("Authorization") != "Bearer human-read-canary" {
			t.Fatal("wrong source or credential", r.Method, r.URL)
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("unbounded inspection")
		}
		headRepo := map[string]any{"id": 77, "full_name": "acme/code"}
		if fork {
			headRepo = map[string]any{"id": 88, "full_name": "outside/code"}
		}
		raw, err := json.Marshal(map[string]any{"id": 99, "number": 42, "title": "Fix this PR", "body": "hidden-description-canary", "state": "open", "head": map[string]any{"ref": "fix", "sha": strings.Repeat("a", 40), "repo": headRepo}, "base": map[string]any{"ref": "main", "sha": strings.Repeat("b", 40), "repo": map[string]any{"id": 77, "full_name": "acme/code"}}})
		if err != nil {
			t.Fatal(err)
		}
		return issueResponse(r, 200, string(raw)), nil
	})
	read, g, p := documentsFixture(t, transport)
	factory := ProviderFactory{SchedulerDirectory: t.TempDir(), Client: &http.Client{Transport: transport}, Registrar: registry}
	service := &PRSelectionService{Permissions: read.Permissions, Client: factory.PRRepair, Scrubber: scrubber}
	value, err := service.Inspect(t.Context(), p, g.Name, "strategy", "42")
	if err != nil || value.Target.SourceID != "99" || value.Target.RepositorySourceID != "77" || value.Target.ExpectedHeadSHA != strings.Repeat("a", 40) || !value.Open || strings.Contains(value.Description, "hidden-description-canary") || reads != 1 {
		t.Fatal(value, reads, err)
	}
	for _, input := range []struct{ gaggle, source, id string }{{"foreign", "strategy", "42"}, {g.Name, "foreign", "42"}, {g.Name, "strategy", "0042"}} {
		if _, err = service.Inspect(t.Context(), p, input.gaggle, input.source, input.id); err == nil || reads != 1 {
			t.Fatal("invalid selection reached provider", input, reads, err)
		}
	}
	fork = true
	if _, err = service.Inspect(t.Context(), p, g.Name, "strategy", "42"); err == nil || reads != 2 {
		t.Fatal("fork was selectable", reads, err)
	}
	g.Spec.InteractiveAccess.Actions = nil
	if err = read.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Inspect(t.Context(), p, g.Name, "strategy", "42"); err == nil || reads != 2 {
		t.Fatal("revoked read reached provider", reads, err)
	}
	var public *httpapi.InterventionError
	if !errors.As(err, &public) || public.Status != http.StatusForbidden {
		t.Fatal("permission failure was not sanitized", err)
	}
}
