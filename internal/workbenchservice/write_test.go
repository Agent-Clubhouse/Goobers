package workbenchservice

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
)

type writeProvider struct {
	mu                      sync.Mutex
	kind                    apiv1.Provider
	title                   string
	reads, patches, version int
	lost                    bool
	patchHook               func(*http.Request)
	t                       *testing.T
}

func (f *writeProvider) roundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(r.Header.Get("Authorization"), "automation") || r.Header.Get("Authorization") == "" {
		f.t.Fatal("missing explicit interactive credential")
	}
	if strings.HasSuffix(r.URL.Path, "/states") {
		return issueResponse(r, 200, `{"value":[{"name":"Active","category":"InProgress"}]}`), nil
	}
	switch r.Method {
	case http.MethodPatch:
		f.patches++
		if f.kind == "github" {
			var patch struct {
				Title string `json:"title"`
			}
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				f.t.Fatal(err)
			}
			f.title = patch.Title
		} else {
			var patch []struct {
				Op, Path string
				Value    json.RawMessage
			}
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				f.t.Fatal(err)
			}
			if len(patch) != 2 || patch[0].Op != "test" || patch[0].Path != "/rev" || string(patch[0].Value) != fmt.Sprint(f.version) || patch[1].Path != "/fields/System.Title" {
				f.t.Fatalf("not an atomic one-field edit: %+v", patch)
			}
			if err := json.Unmarshal(patch[1].Value, &f.title); err != nil {
				f.t.Fatal(err)
			}
		}
		f.version++
		if f.patchHook != nil {
			f.patchHook(r)
		}
		if f.lost {
			return issueResponse(r, 503, `{"message":"sensitive-provider-body"}`), nil
		}
	case http.MethodGet:
		f.reads++
	default:
		f.t.Fatalf("unexpected method %s", r.Method)
	}
	return issueResponse(r, 200, f.body()), nil
}

func (f *writeProvider) body() string {
	title, _ := json.Marshal(f.title)
	if f.kind == "github" {
		return fmt.Sprintf(`{"id":987654,"number":42,"title":%s,"state":"open","html_url":"https://github.com/acme/issues/issues/42","updated_at":"2026-10-04T12:00:%02dZ","labels":[],"assignees":[]}`, title, f.version)
	}
	return fmt.Sprintf(`{"id":42,"rev":%d,"url":"https://dev.azure.com/acme/_apis/wit/workItems/42","fields":{"System.TeamProject":"issues","System.Title":%s,"System.State":"Active","System.WorkItemType":"Feature"},"relations":[]}`, f.version, title)
}

func writerFixture(t *testing.T, kind apiv1.Provider) (*WriterService, *writeProvider, apiv1.Gaggle, httpapi.Principal, workbench.BacklogPatchRequest) {
	t.Helper()
	f := &writeProvider{kind: kind, title: "Old", version: 1, t: t}
	read, g, p := fixture(t, f.roundTrip)
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "backlog.edit")
	g.Spec.Workbench.Sources[0].Writes = &apiv1.WorkbenchWrites{Fields: []apiv1.WorkbenchField{"title", "description"}}
	source := instance.InteractiveCredential{Name: "human", Provider: string(kind), Owner: "acme", Repository: "issues", Token: instance.TokenRef{Env: "HUMAN_BACKLOG"}}
	request := workbench.BacklogPatchRequest{ID: "42", SourceID: "987654", ExpectedRevision: "2026-10-04T12:00:01Z", Field: "title"}
	if kind == "ado" {
		g.Spec.Project = apiv1.RepoRef{Provider: kind, Owner: "acme", Project: "code", Name: "repo"}
		g.Spec.Backlog = apiv1.BacklogRef{Provider: kind, Project: "issues"}
		source.Project, source.Repository = "issues", ""
		request.SourceID, request.ExpectedRevision = "42", "1"
	}
	registry := &secretRegistry{}
	permissions, err := interactiveaccess.New([]apiv1.Gaggle{g}, []instance.InteractiveCredential{source}, interactiveaccess.Dependencies{Registrar: registry})
	if err != nil {
		t.Fatal(err)
	}
	read.Permissions = permissions
	store, err := triggerqueue.Open(filepath.Join(t.TempDir(), "commands.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	value := "Updated by Alice"
	request.Value = &value
	return &WriterService{ReadService: read, Queue: store}, f, g, p, request
}

func TestNativeWriteServiceConfirmedReplayWithoutProviderOrToken(t *testing.T) {
	for _, kind := range []apiv1.Provider{"github", "ado"} {
		t.Run(string(kind), func(t *testing.T) {
			s, f, g, p, request := writerFixture(t, kind)
			capabilities, err := s.Capabilities(t.Context(), p, g.Name, "items")
			if err != nil || len(capabilities.Fields) != 2 || f.reads != 0 {
				t.Fatal(capabilities, err)
			}
			first, err := s.Patch(t.Context(), p, g.Name, "items", "one", request)
			if err != nil || first.State != "confirmed" || first.Receipt == nil || !first.Receipt.ProviderAcknowledged || !first.Receipt.ObservedMatches {
				t.Fatal(first, err)
			}
			reads := f.reads
			t.Setenv("HUMAN_BACKLOG", "")
			g.Spec.Workbench.Sources[0].Objectives = &apiv1.WorkbenchObjectiveSelector{IDs: []string{"different-classification"}}
			if err = s.ReadService.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
				t.Fatal(err)
			}
			replay, err := s.Patch(t.Context(), p, g.Name, "items", "one", request)
			if err != nil || !replay.Duplicate || replay.ID != first.ID || replay.State != "confirmed" {
				t.Fatal(replay, err)
			}
			receipt, err := s.Command(t.Context(), p, g.Name, "items", first.ID)
			if err != nil || receipt.ID != first.ID || f.reads != reads || f.patches != 1 {
				t.Fatal(receipt, err, f.reads, f.patches)
			}
		})
	}
}

func TestNativeWriteServiceLostReplyNeverRetries(t *testing.T) {
	for _, kind := range []apiv1.Provider{"github", "ado"} {
		t.Run(string(kind), func(t *testing.T) {
			s, f, g, p, request := writerFixture(t, kind)
			f.lost = true
			first, err := s.Patch(t.Context(), p, g.Name, "items", "lost", request)
			if err != nil || first.State != "unknown" || first.Receipt == nil || first.Receipt.ProviderAcknowledged || !first.Receipt.ObservedMatches {
				t.Fatal(first, err)
			}
			for range 3 {
				if _, err = s.Patch(t.Context(), p, g.Name, "items", "lost", request); err != nil {
					t.Fatal(err)
				}
			}
			if f.patches != 1 {
				t.Fatal("ambiguous write replayed", f.patches)
			}
			raw, _ := json.Marshal(first)
			if strings.Contains(string(raw), "sensitive-provider-body") || strings.Contains(string(raw), "human-read-canary") {
				t.Fatal("provider failure or token leaked")
			}
		})
	}
}

func expectWriteStatus(t *testing.T, err error, status int) {
	t.Helper()
	var public *httpapi.InterventionError
	if !errors.As(err, &public) || public.Status != status {
		t.Fatalf("want status %d, got %v", status, err)
	}
}

func TestNativeWriteServiceCurrentAuthorityGatesAllReceipts(t *testing.T) {
	s, f, g, p, request := writerFixture(t, "github")
	first, err := s.Patch(t.Context(), p, g.Name, "items", "auth", request)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*apiv1.Gaggle){
		func(g *apiv1.Gaggle) {
			g.Spec.Workbench.Sources[0].Writes.Fields = []apiv1.WorkbenchField{"description"}
		},
		func(g *apiv1.Gaggle) { g.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"backlog.read"} },
		func(g *apiv1.Gaggle) { g.Spec.Backlog.Project = "acme/retargeted" },
	} {
		changed := g.DeepCopy()
		change(changed)
		if err = s.ReadService.Permissions.Apply([]apiv1.Gaggle{*changed}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Command(t.Context(), p, g.Name, "items", first.ID); err == nil {
			t.Fatal("receipt bypassed current authority")
		}
		if _, err = s.Patch(t.Context(), p, g.Name, "items", "auth", request); err == nil {
			t.Fatal("replay bypassed current authority")
		}
	}
	if err = s.ReadService.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	foreign := p
	foreign.Subject = "another-human"
	_, err = s.Command(t.Context(), foreign, g.Name, "items", first.ID)
	expectWriteStatus(t, err, 403)
	changed := request
	value := "Different"
	changed.Value = &value
	_, err = s.Patch(t.Context(), p, g.Name, "items", "auth", changed)
	expectWriteStatus(t, err, 409)
	if f.patches != 1 {
		t.Fatal("denied replay touched provider")
	}
}
