package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/workerhost"
	"github.com/goobers/goobers/providers"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

func fakeBlobPlane(t *testing.T) (string, func(digest string) []byte) {
	t.Helper()
	var mu sync.Mutex
	store := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		if i := strings.LastIndex(key, "/"); i >= 0 {
			key = key[i+1:]
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			body := make([]byte, 0)
			buf := make([]byte, 4096)
			for {
				n, err := r.Body.Read(buf)
				body = append(body, buf[:n]...)
				if err != nil {
					break
				}
			}
			store["sha256:"+key] = body
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			data, ok := store["sha256:"+key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(data)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func(digest string) []byte {
		mu.Lock()
		defer mu.Unlock()
		return store[digest]
	}
}

func manualServiceRootHeader(t *testing.T, root string) string {
	t.Helper()
	id, err := instance.ReadRootIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("Instance root: %q; instance ID: %q\n", filepath.Clean(root), id)
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func replaceInFile(t *testing.T, path, old, replacement string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(raw), old, replacement, 1)
	if updated == string(raw) {
		t.Fatalf("%s does not contain %q", path, old)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
}

func withHarnessAdapter(t *testing.T, lookup func(apiv1.Harness, harness.EnvironmentConfig, map[string][]string, func(context.Context) (string, error)) (harness.Adapter, error)) {
	t.Helper()
	orig := runtimewiring.HarnessAdapterFor
	runtimewiring.HarnessAdapterFor = lookup
	t.Cleanup(func() { runtimewiring.HarnessAdapterFor = orig })
}

func withHarnessProbeThrottle(t *testing.T) *fakeHarnessClock {
	t.Helper()
	clock := &fakeHarnessClock{now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	previous := runtimewiring.HarnessProbeThrottle
	runtimewiring.HarnessProbeThrottle = harness.NewProbeBackoff(clock.Now)
	t.Cleanup(func() { runtimewiring.HarnessProbeThrottle = previous })
	return clock
}

func installCountingHarnessAdapters(t *testing.T, copilotErr error) *countingHarnessAdapters {
	t.Helper()
	adapters := &countingHarnessAdapters{copilotErr: copilotErr, probes: map[apiv1.Harness]int{}}
	withHarnessAdapter(t, func(h apiv1.Harness, _ harness.EnvironmentConfig, _ map[string][]string, credential func(context.Context) (string, error)) (harness.Adapter, error) {
		if credential != nil {
			if _, err := credential(context.Background()); err != nil {
				return nil, err
			}
		}
		adapters.mu.Lock()
		defer adapters.mu.Unlock()
		adapters.probes[h]++
		err := adapters.claudeErr
		if h == apiv1.HarnessCopilot {
			err = adapters.copilotErr
		}
		return &harnesstest.FakeAdapter{AdapterName: string(h), Version: string(h) + " 1.0.0", PreflightErr: err}, nil
	})
	return adapters
}

func rateLimitedPreflight(retryAfter time.Duration) error {
	return &harness.TransientPreflightError{
		Err:        errors.New(`harness: copilot-cli: sign-in check exited 1: Failed to fetch PAT user login (403): API rate limit exceeded`),
		RetryAfter: retryAfter,
	}
}

func declareGitHubAppAuth(t *testing.T, root, slug string) providers.RepositoryRef {
	t.Helper()
	configPath := instance.NewLayout(root).ConfigFile()
	cfg, err := instance.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	ref := githubRepoRefFromConfig(t, cfg)
	for i := range cfg.Repos {
		if cfg.Repos[i].Provider != "github" {
			continue
		}
		cfg.Repos[i].Token = instance.TokenRef{}
		cfg.Repos[i].Auth = &instance.RepoAuthConfig{
			Kind:           instance.GitHubAuthApp,
			AppID:          "123456",
			InstallationID: "42",
			PrivateKey:     &instance.TokenRef{File: "/run/secrets/goobers-app.pem"},
			Slug:           slug,
		}
	}
	if err := instance.WriteConfig(configPath, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	return ref
}

type telemetryParityReader struct {
	*readservice.Telemetry
}

func initDemo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "demo")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	return root
}
func podAgenticExecutorInput(w runtimewiring.PodExecutorWiring) runtimewiring.AgenticExecutorInput {
	input := runtimewiring.PodAgenticExecutorInput(w)
	input.ArtifactRecorder = workerhost.NewStagingArtifacts(w.RunsDir, w.Scrubber, nil)
	return input
}
func buildPodAgenticExecutor(kit *agentickit.Kit, stderr io.Writer, minted []dispatcher.MintedCredential, runsDir string) (invoke.Goober, error) {
	return runtimewiring.BuildPodAgenticExecutor(kit, stderr, minted, runsDir, func(input runtimewiring.AgenticExecutorInput) runtimewiring.AgenticExecutorInput {
		input.ArtifactRecorder = workerhost.NewStagingArtifacts(runsDir, input.SharedRegistry, nil)
		return input
	})
}

type fakeHarnessClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeHarnessClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeHarnessClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

type countingHarnessAdapters struct {
	mu         sync.Mutex
	copilotErr error
	claudeErr  error
	probes     map[apiv1.Harness]int
}

func (a *countingHarnessAdapters) set(copilotErr, claudeErr error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.copilotErr, a.claudeErr = copilotErr, claudeErr
}
func (a *countingHarnessAdapters) count(h apiv1.Harness) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.probes[h]
}

type synchronizedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
func (r *telemetryParityReader) Health(context.Context) (readservice.Health, error) {
	return readservice.Health{Ready: true}, nil
}
func (r *telemetryParityReader) PortalConfig(context.Context) (readservice.PortalConfig, error) {
	return readservice.PortalConfig{}, nil
}
func (r *telemetryParityReader) ListRuns(context.Context, readservice.RunListOptions) (readservice.RunList, error) {
	return readservice.RunList{}, readservice.ErrNotFound
}
func (r *telemetryParityReader) GetRun(context.Context, string) (readservice.RunDetail, error) {
	return readservice.RunDetail{}, readservice.ErrNotFound
}
func (r *telemetryParityReader) RunEvents(context.Context, string) (readservice.EventList, error) {
	return readservice.EventList{}, readservice.ErrNotFound
}
func (r *telemetryParityReader) StageAttempts(context.Context, string, string) (readservice.AttemptList, error) {
	return readservice.AttemptList{}, readservice.ErrNotFound
}
func (r *telemetryParityReader) AddressableAgents(context.Context, string) ([]readservice.AddressableAgent, error) {
	return nil, readservice.ErrNotFound
}
func (r *telemetryParityReader) ResolveAgentAddress(context.Context, string, string) (readservice.AgentResolution, error) {
	return readservice.AgentResolution{}, readservice.ErrNotFound
}
func (r *telemetryParityReader) Artifact(context.Context, string, string) (readservice.ArtifactContent, error) {
	return readservice.ArtifactContent{}, readservice.ErrNotFound
}
func (r *telemetryParityReader) Transcript(context.Context, string, uint64) (readservice.TranscriptContent, error) {
	return readservice.TranscriptContent{}, readservice.ErrNotFound
}
func (r *telemetryParityReader) Instance(context.Context) (readservice.Instance, error) {
	return readservice.Instance{}, nil
}
func (r *telemetryParityReader) Gaggles(context.Context, readservice.PageRequest) (readservice.GagglePage, error) {
	return readservice.GagglePage{}, nil
}
func (r *telemetryParityReader) Goobers(context.Context, string, readservice.PageRequest) (readservice.GooberPage, error) {
	return readservice.GooberPage{}, nil
}
func (r *telemetryParityReader) Workflows(context.Context, string, readservice.PageRequest) (readservice.WorkflowPage, error) {
	return readservice.WorkflowPage{}, nil
}
func (r *telemetryParityReader) Connections(context.Context, string) (readservice.GaggleConnections, error) {
	return readservice.GaggleConnections{}, nil
}
func (r *telemetryParityReader) QueueEligibility(context.Context, string, string) (readservice.QueueEligibilityView, error) {
	return readservice.QueueEligibilityView{}, nil
}
func (r *telemetryParityReader) Workflow(context.Context, string, string) (readservice.WorkflowDetail, error) {
	return readservice.WorkflowDetail{}, nil
}
func (r *telemetryParityReader) RunChildren(context.Context, string, string) (readservice.ChildHistoryPage, error) {
	return readservice.ChildHistoryPage{}, readservice.ErrNotFound
}

func githubRepoRefFromConfig(t *testing.T, cfg *instance.Config) providers.RepositoryRef {
	t.Helper()
	for _, repo := range cfg.Repos {
		if repo.Provider == "github" {
			return providers.RepositoryRef{
				Provider: providers.ProviderGitHub,
				Owner:    repo.Owner,
				Name:     repo.Name,
			}
		}
	}
	t.Fatalf("config declares no github repo")
	return providers.RepositoryRef{}
}

type testCopilotModelLister struct{}

func (testCopilotModelLister) ListModels(context.Context, []string, []string) ([]harness.CopilotModelInfo, error) {
	return []harness.CopilotModelInfo{
		{ID: "claude-fable-5", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-sonnet-5", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-sonnet-4.6", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "max"}},
		{ID: "claude-sonnet-4.5"},
		{ID: "claude-haiku-4.5"},
		{ID: "claude-opus-4.8-fast", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-opus-4.8", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-opus-4.7", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-opus-4.6", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "max"}},
		{ID: "claude-opus-4.5"},
		{ID: "gpt-5.6-sol", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "gpt-5.6-terra", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "gpt-5.6-luna", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "gpt-5.5", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh"}},
		{ID: "gpt-5.4", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh"}},
		{ID: "gpt-5.3-codex", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh"}},
		{ID: "gpt-5.4-mini", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh"}},
		{ID: "gpt-5-mini", SupportedReasoningEfforts: []string{"none", "low", "medium", "high"}},
		{ID: "gemini-3.1-pro-preview", SupportedReasoningEfforts: []string{"none", "low", "medium", "high"}},
		{ID: "gemini-3.5-flash", SupportedReasoningEfforts: []string{"none", "minimal", "low", "medium", "high"}},
		{ID: "kimi-k2.7-code"},
		{ID: "mai-code-1-flash-picker"},
	}, nil
}

func TestMain(m *testing.M) {
	workerExecutorBinary = os.Executable
	runtimewiring.CopilotModelLister = testCopilotModelLister{}
	runtimewiring.PreflightHarnesses = func(map[string]apiv1.GooberSpec, []apiv1.Workflow, harness.EnvironmentConfig, map[string][]string, runtimewiring.ModelCredentialFor) (runtimewiring.HarnessPreflightInfo, error) {
		return runtimewiring.HarnessPreflightInfo{}, nil
	}
	os.Exit(m.Run())
}
