package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func TestRemoteStatusRunsOnlyBoundsDaemonPages(t *testing.T) {
	const instanceID = "0123456789abcdef0123456789abcdef"
	var runRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case apicontract.InstancePath:
			_ = json.NewEncoder(w).Encode(readservice.Instance{InstanceRoot: "/srv/goobers", RootIdentity: &readservice.RootIdentity{ID: instanceID}, Ready: true})
		case apicontract.RunsPath:
			runRequests++
			if got := req.URL.Query(); got.Get("limit") != "3" || got.Get("workflow") != "implementation" || got.Get("gaggle") != "goobers" || got.Get("phase") != "running" {
				t.Errorf("query = %q, want bounded limit and status filters", got.Encode())
			}
			runs := make([]readservice.RunSummary, 3)
			for i := range runs {
				runs[i] = readservice.RunSummary{
					ID: fmt.Sprintf("run-%d", i), Workflow: "implementation", Gaggle: "goobers",
					Phase: journal.PhaseRunning, StartedAt: time.Date(2026, 10, 5, 12-i, 0, 0, 0, time.UTC),
				}
			}
			_ = json.NewEncoder(w).Encode(readservice.RunList{Runs: runs, NextCursor: "more-history"})
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := runStatus([]string{"--api", server.URL, "--runs-only", "--json", "--workflow", "implementation", "--gaggle", "goobers", "--phase", "running", "--limit", "2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	var output statusJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if runRequests != 1 || len(output.Runs) != 2 {
		t.Fatalf("run requests = %d, output runs = %d; want one request and two runs", runRequests, len(output.Runs))
	}
}

func TestRemoteStatusRunsOnlyLimitZeroReadsAllPages(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != apicontract.RunsPath {
			http.NotFound(w, req)
			return
		}
		requests++
		page := readservice.RunList{Runs: []readservice.RunSummary{{
			ID: fmt.Sprintf("run-%d", requests), StartedAt: time.Date(2026, 10, 5-requests, 12, 0, 0, 0, time.UTC),
		}}}
		if requests == 1 {
			page.NextCursor = "next"
		} else if req.URL.Query().Get("cursor") != "next" {
			t.Errorf("cursor = %q, want next", req.URL.Query().Get("cursor"))
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()

	reads := newRemoteRuns(server.URL)
	runs, err := remoteStatusRuns(context.Background(), reads, statusOptions{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(runs) != 2 {
		t.Fatalf("requests = %d, runs = %+v; want both pages", requests, runs)
	}
}

func TestRemoteStatusRunsOnlyBoundsSparseMultiPhasePages(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests++
		query := req.URL.Query()
		if query.Get("phase") != "" || query.Get("limit") != "200" {
			t.Errorf("query = %q, want an unfiltered full page", query.Encode())
		}
		_ = json.NewEncoder(w).Encode(readservice.RunList{
			Runs: []readservice.RunSummary{{
				ID: fmt.Sprintf("run-%d", requests), Phase: journal.PhaseCompleted,
				StartedAt: time.Date(2026, 10, 5-requests, 12, 0, 0, 0, time.UTC),
			}},
			NextCursor: fmt.Sprintf("page-%d", requests+1),
		})
	}))
	defer server.Close()

	options := statusOptions{
		limit: 2,
		phases: map[journal.RunPhase]struct{}{
			journal.PhaseRunning:   {},
			journal.PhaseEscalated: {},
		},
	}
	_, err := remoteStatusRuns(context.Background(), newRemoteRuns(server.URL), options, true)
	if err == nil || !strings.Contains(err.Error(), "remote multi-phase run scan exceeded") {
		t.Fatalf("err = %v, want bounded multi-phase scan error", err)
	}
	if requests != maxRemoteStatusClientFilterPages {
		t.Fatalf("requests = %d, want %d", requests, maxRemoteStatusClientFilterPages)
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
			_ = json.NewEncoder(w).Encode(readservice.EventList{RunID: id, Events: []readservice.RunEvent{
				{Schema: journal.EventSchema, Seq: 1, Type: journal.EventRunStarted, KnownSchema: true, Time: started},
				{Schema: journal.EventSchema, Seq: 2, Type: journal.EventGateOverridden, KnownSchema: true, Time: started, Gate: "review", Verdict: "approve", Actor: "operator", Rationale: "operator approved"},
			}})
		default:
			http.NotFound(w, req)
		}
	}))
}

func TestRemoteArtifactVerifiesServedDigest(t *testing.T) {
	content := []byte("verdict: approve\n")
	digest := journal.Digest(content)
	other := journal.Digest([]byte("something else"))
	for _, tc := range []struct {
		name   string
		header string
		body   []byte
		want   string
	}{
		{name: "verified", header: digest, body: content},
		{name: "substituted header", header: other, body: content, want: "served"},
		{name: "missing header", header: "", body: content, want: "served"},
		{name: "tampered body", header: digest, body: []byte("verdict: reject\n"), want: "digest verification"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if tc.header != "" {
					w.Header().Set(apicontract.DigestHeader, tc.header)
				}
				_, _ = w.Write(tc.body)
			}))
			defer server.Close()
			got, err := newRemoteRuns(server.URL).Artifact(context.Background(), remoteReadTestID, digest)
			if tc.want == "" {
				if err != nil || !bytes.Equal(got.Bytes, content) || got.Metadata.Digest != digest {
					t.Fatalf("Artifact = %+v, %v", got, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestRemoteArtifactRefusesMalformedDigestBeforeRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a malformed digest must not reach the daemon")
	}))
	defer server.Close()
	if _, err := newRemoteRuns(server.URL).Artifact(context.Background(), remoteReadTestID, "../../etc/passwd"); err == nil {
		t.Fatal("expected malformed digest to be refused")
	}
}

func TestRemoteTranscriptRefusesAnotherSequence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Goobers-Event-Sequence", "8")
		_, _ = w.Write([]byte("transcript"))
	}))
	defer server.Close()
	_, err := newRemoteRuns(server.URL).Transcript(context.Background(), remoteReadTestID, 7)
	if err == nil || !strings.Contains(err.Error(), "seq 7") {
		t.Fatalf("err = %v, want a sequence mismatch refusal", err)
	}
}

func TestRemoteReadRefusesDaemonIdentityChangeMidRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(api string) []string
		run  func([]string, io.Writer, io.Writer) int
	}{
		{"escalations", func(api string) []string { return []string{"--api", api, "--json"} }, runEscalations},
		{"escalations show", func(api string) []string { return []string{"--api", api, "--json", remoteReadTestID} }, runEscalationShow},
		{"status", func(api string) []string { return []string{"--api", api, "--json"} }, runStatus},
		{"trace", func(api string) []string { return []string{"--api", api, "--json", remoteReadTestID} }, runTrace},
		{"trace transcripts", func(api string) []string { return []string{"--api", api, "--transcripts", remoteReadTestID} }, runTrace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := remoteReadProxy(t, remoteReadTestServer(t, remoteReadTestID), true, nil)
			var stdout, stderr bytes.Buffer
			if code := tc.run(tc.args(server.URL), &stdout, &stderr); code != 2 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), "changed during the read") {
				t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
			}
		})
	}
}

// remoteReadProxy forwards to inner. With swapIdentity it answers every
// /instance read after the first as a different instance, standing in for a
// daemon replaced behind the endpoint after the command validated it. observe,
// when set, sees every request path.
func remoteReadProxy(t *testing.T, inner *httptest.Server, swapIdentity bool, observe func(path string)) *httptest.Server {
	t.Helper()
	t.Cleanup(inner.Close)
	instanceCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if observe != nil {
			observe(req.URL.Path)
		}
		if req.URL.Path == apicontract.InstancePath {
			instanceCalls++
			if swapIdentity && instanceCalls > 1 {
				_ = json.NewEncoder(w).Encode(readservice.Instance{InstanceRoot: "/srv/other", RootIdentity: &readservice.RootIdentity{ID: "fedcba9876543210fedcba9876543210"}, Ready: true})
				return
			}
		}
		proxy, err := http.Get(inner.URL + req.URL.RequestURI())
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = proxy.Body.Close() }()
		for key, values := range proxy.Header {
			w.Header()[key] = values
		}
		w.WriteHeader(proxy.StatusCode)
		_, _ = io.Copy(w, proxy.Body)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestRemoteTraceRefusesFollow(t *testing.T) {
	server := remoteReadTestServer(t, remoteReadTestID)
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if code := runTrace([]string{"--api", server.URL, "--follow", remoteReadTestID}, &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--follow is not supported with --api") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRemoteTraceCarriesServedEventFields(t *testing.T) {
	server := remoteReadTestServer(t, remoteReadTestID)
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if code := runTrace([]string{"--api", server.URL, "--json", remoteReadTestID}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	var result struct {
		Events []struct {
			Rationale string `json:"rationale"`
		} `json:"events"`
		State *struct {
			MachineState string `json:"machineState"`
		} `json:"state"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode trace: %v\n%s", err, stdout.String())
	}
	var found bool
	for _, event := range result.Events {
		found = found || event.Rationale == "operator approved"
	}
	if !found {
		t.Fatalf("events = %+v, want the served rationale carried through", result.Events)
	}
	if result.State != nil && result.State.MachineState != "" {
		t.Fatalf("machineState = %q; the API does not serve it, so it must not be approximated", result.State.MachineState)
	}
}

func TestRemoteRunIDResolvesFullIDWithoutPagingRuns(t *testing.T) {
	listed := false
	server := remoteReadProxy(t, remoteReadTestServer(t, remoteReadTestID), false, func(path string) {
		listed = listed || path == apicontract.RunsPath
	})
	reads := newRemoteRuns(server.URL)
	id, err := resolveRemoteRunID(context.Background(), reads, remoteReadTestID)
	if err != nil || id != remoteReadTestID || listed {
		t.Fatalf("id = %q, err = %v, listed = %v", id, err, listed)
	}
	id, err = resolveRemoteRunID(context.Background(), reads, remoteReadTestID[:8])
	if err != nil || id != remoteReadTestID {
		t.Fatalf("prefix resolution: id = %q, err = %v", id, err)
	}
}

func TestInheritedDaemonAPIDoesNotBreakLocalDaemonProbe(t *testing.T) {
	t.Setenv("GOOBERS_DAEMON_API", "http://127.0.0.1:1")
	var stdout, stderr bytes.Buffer
	_ = runStatus([]string{"--daemon", t.TempDir()}, &stdout, &stderr)
	if strings.Contains(stderr.String(), "--api cannot be combined") {
		t.Fatalf("an inherited $GOOBERS_DAEMON_API must not refuse the local --daemon probe: %q", stderr.String())
	}
	stderr.Reset()
	if code := runStatus([]string{"--api", "http://127.0.0.1:1", "--daemon"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--api cannot be combined") {
		t.Fatalf("an explicit --api with --daemon must still be refused: code = %d, stderr = %q", code, stderr.String())
	}
}

func TestRemoteReadRefusesDaemonWithoutUsableIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(w).Encode(readservice.Instance{InstanceRoot: "/srv/goobers", Ready: true})
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if code := runEscalations([]string{"--api", server.URL}, &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
}

func TestRemoteTraceAndStatusNameWhatTheyOmit(t *testing.T) {
	server := remoteReadTestServer(t, remoteReadTestID)
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if code := runTrace([]string{"--api", server.URL, "--json", remoteReadTestID}, &stdout, &stderr); code != 0 {
		t.Fatalf("trace code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), remoteTraceOmissionNote) {
		t.Fatalf("trace stderr = %q, want the omission note", stderr.String())
	}
	if strings.Contains(stdout.String(), "note:") {
		t.Fatalf("the omission note must stay off stdout so --json remains parseable: %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runStatus([]string{"--api", server.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("status code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), remoteStatusOmissionNote) {
		t.Fatalf("status stderr = %q, want the omission note", stderr.String())
	}
}
