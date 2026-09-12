package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

const remoteReadTestID = "0123456789abcdef0123456789abcdef"

func TestRemoteRunsUsesVersionedRoutesFiltersAndBearerToken(t *testing.T) {
	t.Setenv("GOOBERS_API_TOKEN", "read-token")
	started := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != apicontract.RunsPath {
			t.Errorf("path = %q", req.URL.Path)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer read-token" {
			t.Errorf("Authorization = %q", got)
		}
		want := url.Values{"phase": {"escalated"}, "workflow": {"repair"}, "limit": {"17"}, "showNoWork": {"true"}}
		if got := req.URL.Query(); got.Encode() != want.Encode() {
			t.Errorf("query = %q, want %q", got.Encode(), want.Encode())
		}
		_ = json.NewEncoder(w).Encode(readservice.RunList{Runs: []readservice.RunSummary{{ID: remoteReadTestID, Workflow: "repair", Phase: journal.PhaseEscalated, StartedAt: started}}})
	}))
	defer server.Close()

	page, err := newRemoteRuns(server.URL).ListRuns(context.Background(), readservice.RunListOptions{Workflow: "repair", Phase: journal.PhaseEscalated, Limit: 17, ShowNoWork: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 1 || page.Runs[0].ID != remoteReadTestID {
		t.Fatalf("runs = %+v", page.Runs)
	}
}

func TestRemoteEscalationsUsesDaemonWithoutLocalRoot(t *testing.T) {
	server := remoteReadTestServer(t, remoteReadTestID)
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if code := runEscalations([]string{"--api", server.URL, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), remoteReadTestID) {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Remote instance root") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRemoteReadRefusesExplicitMismatchedRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "instance.yaml"), []byte("schema: goobers.dev/instance/v1alpha1\nname: test\nenvironment: development\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.EnsureRootIdentity(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	server := remoteReadTestServer(t, remoteReadTestID)
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if code := runEscalations([]string{"--api", server.URL, root}, &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "another instance") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRemoteStatusFiltersUsingSharedRenderer(t *testing.T) {
	server := remoteReadTestServer(t, remoteReadTestID)
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if code := runStatus([]string{"--api", server.URL, "--phase", "escalated"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), remoteReadTestID) || !strings.Contains(stdout.String(), "repair") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRemoteRunsDirectVerbUsesRunTable(t *testing.T) {
	server := remoteReadTestServer(t, remoteReadTestID)
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"runs", "--api", server.URL, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), remoteReadTestID) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRemoteTraceUsesDaemonWithoutLocalJournal(t *testing.T) {
	server := remoteReadTestServer(t, remoteReadTestID)
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if code := runTrace([]string{"--api", server.URL, remoteReadTestID}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "run:      "+remoteReadTestID) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func remoteReadTestServer(t *testing.T, id string) *httptest.Server {
	t.Helper()
	started := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	summary := readservice.RunSummary{ID: id, Workflow: "repair", Gaggle: "product", Phase: journal.PhaseEscalated, Terminal: true, StartedAt: started, LastActivityAt: started}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case apicontract.InstancePath:
			_ = json.NewEncoder(w).Encode(readservice.Instance{InstanceRoot: "/srv/goobers", RootIdentity: &readservice.RootIdentity{ID: id}, Ready: true})
		case apicontract.RunsPath:
			_ = json.NewEncoder(w).Encode(readservice.RunList{Runs: []readservice.RunSummary{summary}})
		case strings.ReplaceAll(apicontract.RunDetailPath, "{run}", id):
			_ = json.NewEncoder(w).Encode(readservice.RunDetail{RunSummary: summary, Escalation: &readservice.EscalationCause{RepassCount: 2}})
		case strings.ReplaceAll(apicontract.RunEventsPath, "{run}", id):
			_ = json.NewEncoder(w).Encode(readservice.EventList{RunID: id, Events: []readservice.RunEvent{{Schema: journal.EventSchema, Seq: 1, Type: journal.EventRunStarted, KnownSchema: true, Time: started}}})
		default:
			http.NotFound(w, req)
		}
	}))
}
