package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	stdlog "log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/providers"
)

type publicationObservingClient struct {
	base      providers.HTTPClient
	published func(*http.Request)
}

func (c publicationObservingClient) Do(r *http.Request) (*http.Response, error) {
	c.published(r)
	return c.base.Do(r)
}

func TestPublicationLeakScreenRequiresExplicitShadowOptIn(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": map[string]any{
				decisiongate.PublicationLeakQuestion: map[string]any{"type": "noul", "noul": 0.95},
			},
		})
	}))
	t.Cleanup(server.Close)
	t.Setenv("LEAK_SCREEN_URL", server.URL)
	t.Setenv("LEAK_SCREEN_KEY", "test-key")
	t.Setenv("LEAK_SCREEN_MODEL", "test-model")

	settings := &decisiongate.Settings{
		BaseURLEnv: "LEAK_SCREEN_URL", KeyEnv: "LEAK_SCREEN_KEY", ModelEnv: "LEAK_SCREEN_MODEL",
		Fallback: decisiongate.FallbackAgent,
	}
	for _, mode := range []decisiongate.Mode{"", decisiongate.ModeOff, decisiongate.ModeEnforce} {
		settings.Mode = mode
		screen := newPublicationLeakScreen(&instance.Config{DecisionGate: settings}, nil)
		if screen != nil {
			screen(context.Background(), "issue", "title", "body")
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("off or enforce mode sent %d requests", calls.Load())
	}

	settings.Mode = decisiongate.ModeShadow
	if screen := newPublicationLeakScreen(&instance.Config{DecisionGate: settings}, nil); screen != nil {
		screen(context.Background(), "issue", "title", "body")
	}
	if calls.Load() != 0 {
		t.Fatalf("shadow mode without publication opt-in sent %d requests", calls.Load())
	}

	var logs bytes.Buffer
	settings.PublicationLeakScreen = true
	screen := newPublicationLeakScreen(
		&instance.Config{DecisionGate: settings},
		slog.New(slog.NewTextHandler(&logs, nil)),
	)
	if screen == nil {
		t.Fatal("shadow opt-in did not create a screen")
	}
	screen(context.Background(), "issue", "title", "body")
	if calls.Load() != 1 {
		t.Fatalf("explicit publication opt-in sent %d requests, want 1", calls.Load())
	}
	if got := logs.String(); !strings.Contains(got, "decisiongate.publication-shadow") ||
		strings.Contains(got, "title") || strings.Contains(got, "body") {
		t.Fatalf("unexpected advisory log: %q", got)
	}
}

func TestOptedInPodPublicationScreensBeforePublishing(t *testing.T) {
	var sequence atomic.Int32
	var screenedAt atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		screenedAt.Store(sequence.Add(1))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": map[string]any{
				decisiongate.PublicationLeakQuestion: map[string]any{"type": "noul", "noul": 0.95},
			},
		})
	}))
	t.Cleanup(server.Close)

	settings := &decisiongate.Settings{
		Mode: decisiongate.ModeShadow, PublicationLeakScreen: true,
		BaseURLEnv: "LEAK_SCREEN_URL", KeyEnv: "LEAK_SCREEN_KEY", ModelEnv: "LEAK_SCREEN_MODEL",
		Fallback: decisiongate.FallbackAgent,
	}
	t.Setenv("LEAK_SCREEN_URL", server.URL)
	t.Setenv("LEAK_SCREEN_KEY", "pod-test-key")
	t.Setenv("LEAK_SCREEN_MODEL", "pod-test-model")
	service := daemonCredentialService{config: &instance.Config{DecisionGate: settings}}
	delivery := service.publicationLeakScreenDelivery(
		httpapi.CredentialResolveRequest{Capabilities: []string{string(capability.ProviderPRWrite)}},
		stageResolution{profile: stageProfile{deterministic: true}},
	)
	if delivery == nil {
		t.Fatal("daemon did not deliver the opted-in publication screen")
	}
	if got := service.publicationLeakScreenDelivery(
		httpapi.CredentialResolveRequest{Capabilities: []string{string(capability.GitHubIssuesRead)}},
		stageResolution{profile: stageProfile{deterministic: true}},
	); got != nil {
		t.Fatal("daemon delivered model access to a stage without publication authority")
	}
	creds := podStageCredentials{publicationLeakScreen: &dispatcher.PublicationLeakScreen{
		Settings: delivery.Settings, BaseURL: delivery.BaseURL, APIKey: delivery.APIKey, Model: delivery.Model,
	}}
	if scrubbed := creds.withGrant(nil); len(scrubbed) != 1 || scrubbed[0].Value != "pod-test-key" {
		t.Fatalf("publication model key was not registered for pod output scrubbing: %+v", scrubbed)
	}
	t.Setenv(dispatcher.EnvPodAttempt, "1")
	for _, entry := range creds.env() {
		name, value, _ := strings.Cut(entry, "=")
		if name == dispatcher.EnvPublicationLeakScreen {
			t.Setenv(name, value)
		}
	}

	screen := publicationLeakScreenForRoot(t.TempDir(), nil)
	if screen == nil {
		t.Fatal("pod invocation did not install the delivered publication screen")
	}
	screen(context.Background(), "pull-request", "title", "body")
	publishedAt := sequence.Add(1)
	if screenedAt.Load() == 0 || screenedAt.Load() >= publishedAt {
		t.Fatalf("screened at %d, published at %d; want screening first", screenedAt.Load(), publishedAt)
	}
}

func TestPublicationLeakScreenCredentialPlaneRoundTrip(t *testing.T) {
	settings := &decisiongate.Settings{
		Mode: decisiongate.ModeShadow, PublicationLeakScreen: true,
		BaseURLEnv: "LEAK_SCREEN_URL", KeyEnv: "LEAK_SCREEN_KEY", ModelEnv: "LEAK_SCREEN_MODEL",
		Fallback: decisiongate.FallbackAgent,
	}
	t.Setenv("LEAK_SCREEN_URL", "https://screen.example.test/v1")
	t.Setenv("LEAK_SCREEN_KEY", "plane-test-key")
	t.Setenv("LEAK_SCREEN_MODEL", "plane-test-model")

	spec := credentialPlaneSpec()
	spec.Tasks[1].Capabilities = append(spec.Tasks[1].Capabilities, string(capability.ProviderPRWrite))
	service, _, runID := newCredentialPlaneFixture(t, compileCredentialPlaneMachine(t, spec))
	service.config.DecisionGate = settings

	registry := podauth.NewRegistry()
	authenticator, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(
		&telemetryParityReader{},
		httpapi.RequireRoles(),
		stdlog.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(authenticator),
		httpapi.WithCredentialService(service),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	token, err := registry.Mint(runID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	client := &dispatcher.CredentialResolveClient{BaseURL: server.URL, Token: token}

	resolution, err := client.ResolveStage(
		context.Background(),
		runID,
		"push-branch",
		[]string{string(capability.ProviderPRWrite)},
	)
	if err != nil {
		t.Fatalf("resolve publication capability: %v", err)
	}
	screen := resolution.PublicationLeakScreen
	if screen == nil {
		t.Fatal("opted-in publication capability did not reach CredentialResolution")
	}
	if !reflect.DeepEqual(screen.Settings, *settings) ||
		screen.BaseURL != "https://screen.example.test/v1" ||
		screen.APIKey != "plane-test-key" ||
		screen.Model != "plane-test-model" {
		t.Fatalf("publication screen = %+v, want configured settings, endpoint, key, and model", screen)
	}

	resolution, err = client.ResolveStage(context.Background(), runID, "push-branch", []string{"repo:push"})
	if err != nil {
		t.Fatalf("resolve non-publication capability: %v", err)
	}
	if resolution.PublicationLeakScreen != nil {
		t.Fatalf("non-publication resolve received screen: %+v", resolution.PublicationLeakScreen)
	}
}

func TestPodPublicationCommandsScreenBeforeProviderWrite(t *testing.T) {
	for _, command := range []string{"open-pr", "file-issues"} {
		for _, optedIn := range []bool{false, true} {
			name := "unopted"
			if optedIn {
				name = "opted-in"
			}
			t.Run(command+"/"+name, func(t *testing.T) {
				var sequence atomic.Int32
				var screenedAt atomic.Int32
				var publishedAt atomic.Int32
				var modelPayload atomic.Value
				model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					payload, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read model request: %v", err)
					}
					modelPayload.Store(string(payload))
					screenedAt.Store(sequence.Add(1))
					_ = json.NewEncoder(w).Encode(map[string]any{
						"answers": map[string]any{
							decisiongate.PublicationLeakQuestion: map[string]any{"type": "noul", "noul": 0.95},
						},
					})
				}))
				t.Cleanup(model.Close)
				setPodPublicationLeakScreen(t, model.URL, optedIn)

				var publishPath string
				var wantPayload string
				switch command {
				case "open-pr":
					root := initDemo(t)
					server := newFakeGitHubServer(t, "your-org", "your-repo")
					providerCmdEnv(t, server, capability.CredentialEnvVar(string(capability.ProviderPRWrite)), "publication-screen-run")
					publishPath = "/pulls"
					wantPayload = `"kind":"pull-request"`
					observePublicationWrite(t, publishPath, &sequence, &publishedAt)
					t.Chdir(t.TempDir())
					if code, stdout, stderr := runArgs(t, "open-pr", root); code != 0 {
						t.Fatalf("open-pr: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
					}
				case "file-issues":
					fixture := newFileIssuesFixture(t)
					nomination := lowRisk("publication-screen")
					fixture.writeArtifact(nomination)
					publishPath = "/issues"
					wantPayload = `"kind":"issue"`
					observePublicationWrite(t, publishPath, &sequence, &publishedAt)
					fixture.mustRun()
				}

				if publishedAt.Load() == 0 {
					t.Fatalf("%s did not reach provider write %s", command, publishPath)
				}
				if !optedIn {
					if screenedAt.Load() != 0 {
						t.Fatalf("unopted %s sent publication text to the model", command)
					}
					return
				}
				if screenedAt.Load() != 1 || publishedAt.Load() != 2 {
					t.Fatalf("%s screened at %d and published at %d; want 1 then 2", command, screenedAt.Load(), publishedAt.Load())
				}
				payload, _ := modelPayload.Load().(string)
				if !strings.Contains(payload, wantPayload) {
					t.Fatalf("%s model request did not contain publication kind %s: %s", command, wantPayload, payload)
				}
			})
		}
	}
}

func setPodPublicationLeakScreen(t *testing.T, modelURL string, optedIn bool) {
	t.Helper()
	t.Setenv(dispatcher.EnvPodAttempt, "1")
	t.Setenv(dispatcher.EnvPublicationLeakScreen, "")
	if !optedIn {
		return
	}
	delivery := dispatcher.PublicationLeakScreen{
		Settings: decisiongate.Settings{
			Mode: decisiongate.ModeShadow, PublicationLeakScreen: true,
			BaseURLEnv: "LEAK_SCREEN_URL", KeyEnv: "LEAK_SCREEN_KEY", ModelEnv: "LEAK_SCREEN_MODEL",
			Fallback: decisiongate.FallbackAgent,
		},
		BaseURL: modelURL,
		APIKey:  "pod-test-key",
		Model:   "pod-test-model",
	}
	encoded, err := json.Marshal(delivery)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(dispatcher.EnvPublicationLeakScreen, string(encoded))
}

func observePublicationWrite(t *testing.T, path string, sequence, publishedAt *atomic.Int32) {
	t.Helper()
	baseFactory := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		provider := baseFactory(token, opts...)
		provider.Client = publicationObservingClient{
			base: provider.Client,
			published: func(r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, path) {
					publishedAt.Store(sequence.Add(1))
				}
			},
		}
		return provider
	}
	t.Cleanup(func() { newGitHubProvider = baseFactory })
}
