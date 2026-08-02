package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// resolveGrants materializes each grant's ref through the resolver, returning a
// capability->token-value map so tests can assert which token actually backs a
// capability (the whole point of #287's per-capability sourcing/override).
func resolveGrants(t *testing.T, r credentials.Resolver, grants []credentials.Grant) map[string]string {
	t.Helper()
	out := make(map[string]string, len(grants))
	for _, g := range grants {
		if _, dup := out[g.Capability]; dup {
			t.Fatalf("capability %q granted more than once: %+v", g.Capability, grants)
		}
		val, err := r.Resolve(context.Background(), g.Ref)
		if err != nil {
			t.Fatalf("resolve ref %q for %q: %v", g.Ref, g.Capability, err)
		}
		out[g.Capability] = val
	}
	return out
}

type runnerWiringModelLister struct {
	responses [][]harness.CopilotModelInfo
	env       []string
	calls     int
}

func (l *runnerWiringModelLister) ListModels(_ context.Context, _ []string, env []string) ([]harness.CopilotModelInfo, error) {
	l.env = append([]string(nil), env...)
	response := l.responses[min(l.calls, len(l.responses)-1)]
	l.calls++
	return append([]harness.CopilotModelInfo(nil), response...), nil
}

type runnerWiringHarnessRecorder struct {
	dir string
}

func (r runnerWiringHarnessRecorder) RecordArtifact(string, []byte) (journal.Ref, error) {
	return journal.ArtifactRef(nil)
}

func (r runnerWiringHarnessRecorder) RecordSpanWithSchema(_, _, _ string, data []byte) (journal.Ref, error) {
	return journal.ArtifactRef(data)
}

func (r runnerWiringHarnessRecorder) Dir() string {
	return r.dir
}

func TestResolveOTLPHeaders(t *testing.T) {
	t.Setenv("OTLP_AUTHORIZATION", "Bearer collector-secret")
	registry := journal.NewRegistryScrubber()
	headers, err := resolveOTLPHeaders(context.Background(), map[string]instance.TokenRef{
		"authorization": {Env: "OTLP_AUTHORIZATION"},
	}, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := headers["authorization"]; got != "Bearer collector-secret" {
		t.Fatalf("authorization header = %q, want resolved environment value", got)
	}
	if got := string(registry.Scrub([]byte("credential: Bearer collector-secret"))); strings.Contains(got, "collector-secret") {
		t.Fatalf("resolved collector credential was not registered for redaction: %q", got)
	}
}

func TestResolveOTLPHeadersFailsOnMissingCredential(t *testing.T) {
	_, err := resolveOTLPHeaders(context.Background(), map[string]instance.TokenRef{
		"authorization": {Env: "UNSET_OTLP_AUTHORIZATION"},
	}, journal.NewRegistryScrubber(), nil)
	if err == nil || !strings.Contains(err.Error(), `env var "UNSET_OTLP_AUTHORIZATION" is not set`) {
		t.Fatalf("expected missing collector credential error, got %v", err)
	}
}

// wiringFakeStoreResolver stands in for the secretstore registry at the
// cmd-level seams.
type wiringFakeStoreResolver map[string]string

func (f wiringFakeStoreResolver) FetchSecret(_ context.Context, ref string) (string, error) {
	value, ok := f[ref]
	if !ok {
		return "", fmt.Errorf("secretstore: store ref %q is not declared", ref)
	}
	return value, nil
}

// TestResolveOTLPHeadersStoreRef pins #683's parity contract at a
// representative consumer: a store-backed ref resolves exactly where env/file
// refs do, and the resolved value is registered with the journal scrubber the
// same way.
func TestResolveOTLPHeadersStoreRef(t *testing.T) {
	registry := journal.NewRegistryScrubber()
	headers, err := resolveOTLPHeaders(context.Background(), map[string]instance.TokenRef{
		"authorization": {Store: "prod-kv/otlp-authorization"},
	}, registry, wiringFakeStoreResolver{"prod-kv/otlp-authorization": "Bearer store-collector-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if got := headers["authorization"]; got != "Bearer store-collector-secret" {
		t.Fatalf("authorization header = %q, want store-resolved value", got)
	}
	if got := string(registry.Scrub([]byte("credential: Bearer store-collector-secret"))); strings.Contains(got, "store-collector-secret") {
		t.Fatalf("store-resolved collector credential was not registered for redaction: %q", got)
	}

	// Without a store resolver the same ref fails closed instead of reading
	// as an unconfigured header.
	if _, err := resolveOTLPHeaders(context.Background(), map[string]instance.TokenRef{
		"authorization": {Store: "prod-kv/otlp-authorization"},
	}, journal.NewRegistryScrubber(), nil); err == nil {
		t.Fatal("resolveOTLPHeaders: want fail-closed error for store ref without a store resolver, got nil")
	}
}

type runnerWiringOTLPCollector struct {
	collectortrace.UnimplementedTraceServiceServer
	requests chan *collectortrace.ExportTraceServiceRequest
}

func (c *runnerWiringOTLPCollector) Export(
	_ context.Context,
	req *collectortrace.ExportTraceServiceRequest,
) (*collectortrace.ExportTraceServiceResponse, error) {
	c.requests <- req
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

type unavailableRunnerWiringOTLPCollector struct {
	collectortrace.UnimplementedTraceServiceServer
}

func (unavailableRunnerWiringOTLPCollector) Export(
	context.Context,
	*collectortrace.ExportTraceServiceRequest,
) (*collectortrace.ExportTraceServiceResponse, error) {
	return nil, status.Error(codes.Unavailable, "collector unavailable")
}

func TestBuildTelemetryClientScrubsRegisteredSecretFromOTLP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	collector := &runnerWiringOTLPCollector{
		requests: make(chan *collectortrace.ExportTraceServiceRequest, 1),
	}
	collectortrace.RegisterTraceServiceServer(server, collector)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	const secret = "purple umbrella seven"
	t.Setenv("RUNNERWIRING_OTLP_SECRET", secret)
	registry := journal.NewRegistryScrubber()
	scrubber := journal.Chain(registry, journal.NewPatternScrubber())
	client, err := buildTelemetryClient(
		context.Background(),
		instance.NewLayout(t.TempDir()),
		scrubber,
		registry,
		instance.OTLPConfig{
			Endpoint: "http://" + listener.Addr().String(),
			Insecure: true,
			Headers: map[string]instance.TokenRef{
				"authorization": {Env: "RUNNERWIRING_OTLP_SECRET"},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	shutdown := false
	t.Cleanup(func() {
		if !shutdown {
			_ = client.Shutdown(context.Background())
		}
	})

	_, span, err := client.StartRun(context.Background(), telemetry.RunAttributes{
		Gaggle:     "acme-web",
		WorkflowID: "wf",
		RunID:      "0af7651916cd43dd8448eb211c80319c",
	})
	if err != nil {
		t.Fatal(err)
	}
	span.Fail(errors.New("collector failure: " + secret))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	shutdown = true

	select {
	case req := <-collector.requests:
		raw, err := proto.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("registered collector credential appeared in exported span data")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("collector did not receive an OTLP export")
	}
}

func TestIngestRunTelemetryDoesNotWaitForUnavailableOTLPCollector(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	collectortrace.RegisterTraceServiceServer(server, unavailableRunnerWiringOTLPCollector{})
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	journalExporter := telemetry.NewMemoryExporter()
	client, err := telemetry.New(context.Background(), telemetry.Config{
		ServiceName:  "telemetry-test",
		SpanExporter: journalExporter,
		Exporter:     telemetry.ExporterOTLP,
		OTLPEndpoint: listener.Addr().String(),
		OTLPInsecure: true,
		Batch:        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_ = client.Shutdown(ctx)
	})

	_, span, err := client.StartRun(context.Background(), telemetry.RunAttributes{
		Gaggle:     "acme-web",
		WorkflowID: "wf",
		RunID:      "0af7651916cd43dd8448eb211c80319c",
	})
	if err != nil {
		t.Fatal(err)
	}
	span.End()

	done := make(chan struct{})
	go func() {
		ingestRunTelemetry(client, nil, nil, instance.Layout{}, "", nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("run telemetry ingest waited for unavailable OTLP collector")
	}
	if got := len(journalExporter.Spans()); got != 1 {
		t.Fatalf("local journal exporter spans = %d, want 1", got)
	}
}

// TestBuildEnvCapabilities is #288's wiring: the Copilot adapter's capability→
// env-var map routes agent:model to COPILOT_GITHUB_TOKEN, the nomination
// approval and milestone-command authorities to dedicated GOOBERS_CRED_*
// variables, and other org-repo capabilities to GH_TOKEN.
func TestBuildEnvCapabilities(t *testing.T) {
	envCaps := buildEnvCapabilities()
	if got := envCaps["agent:model"]; got != "COPILOT_GITHUB_TOKEN" {
		t.Fatalf("agent:model env = %q, want COPILOT_GITHUB_TOKEN", got)
	}
	for _, c := range credentialedCapabilities {
		want := credentialGrantEnv
		if c == capability.GitHubIssuesApprove || c == capability.GitHubMilestonesWrite {
			want = executor.CredentialEnvVar(string(c))
		}
		if got := envCaps[string(c)]; got != want {
			t.Fatalf("capability %s env = %q, want %q", c, got, want)
		}
	}
	if envCaps["agent:model"] == credentialGrantEnv {
		t.Fatalf("agent:model must map to a var distinct from the github-tool var %q, else the two tokens collide", credentialGrantEnv)
	}
	if got := envCaps["github:issues:approve"]; got != "GOOBERS_CRED_GITHUB_ISSUES_APPROVE" {
		t.Fatalf("github:issues:approve env = %q, want dedicated approval variable", got)
	}
	if got := envCaps["github:milestones:write"]; got != "GOOBERS_CRED_GITHUB_MILESTONES_WRITE" {
		t.Fatalf("github:milestones:write env = %q, want dedicated milestone variable", got)
	}
}

func TestBuildHarnessRegistryMapsGooberHarnessesToAdapters(t *testing.T) {
	envCaps := buildEnvCapabilities()
	registry, err := buildHarnessRegistry(envCaps, nil, "/instances/acme", "/opt/goobers/bin/goobers")
	if err != nil {
		t.Fatalf("buildHarnessRegistry: %v", err)
	}
	adapter, err := registry.Get(string(apiv1.HarnessCopilot))
	if err != nil {
		t.Fatalf("Get(copilot): %v", err)
	}
	copilot, ok := adapter.(*harness.CopilotAdapter)
	if !ok {
		t.Fatalf("registered adapter = %T, want *harness.CopilotAdapter", adapter)
	}
	if copilot.Name() != "copilot-cli" {
		t.Fatalf("adapter Name = %q, want existing diagnostic identity copilot-cli", copilot.Name())
	}
	if copilot.EnvCapabilities[string(capability.AgentModel)] != copilotModelEnv {
		t.Fatalf("agent:model env = %q, want %q", copilot.EnvCapabilities[string(capability.AgentModel)], copilotModelEnv)
	}
	if !copilot.OptionalCredentialCapabilities[string(capability.AgentModel)] {
		t.Fatal("agent:model must allow stored Copilot CLI authentication when no token grant is configured")
	}
	if len(copilot.AuthCheckArgs) == 0 {
		t.Fatal("registered Copilot adapter is missing its authentication preflight")
	}
	if copilot.InstanceRoot != "/instances/acme" {
		t.Fatalf("adapter instance root = %q, want /instances/acme", copilot.InstanceRoot)
	}
	if copilot.SelfBin != "/opt/goobers/bin/goobers" {
		t.Fatalf("adapter self binary = %q, want /opt/goobers/bin/goobers", copilot.SelfBin)
	}

	adapter, err = registry.Get(string(apiv1.HarnessClaudeCode))
	if err != nil {
		t.Fatalf("Get(claude-code): %v", err)
	}
	claude, ok := adapter.(*harness.ClaudeAdapter)
	if !ok {
		t.Fatalf("registered adapter = %T, want *harness.ClaudeAdapter", adapter)
	}
	if claude.Name() != "claude-code" {
		t.Fatalf("adapter Name = %q, want claude-code", claude.Name())
	}
	if claude.EnvCapabilities[string(capability.AgentModel)] != claudeModelEnv {
		t.Fatalf("agent:model env = %q, want %q", claude.EnvCapabilities[string(capability.AgentModel)], claudeModelEnv)
	}
	if !claude.OptionalCredentialCapabilities[string(capability.AgentModel)] {
		t.Fatal("agent:model must allow stored Claude Code authentication when no token grant is configured")
	}
	if claude.EnvCapabilities[string(capability.GitHubIssuesWrite)] != credentialGrantEnv {
		t.Fatalf("github:issues:write env = %q, want %q", claude.EnvCapabilities[string(capability.GitHubIssuesWrite)], credentialGrantEnv)
	}
	if claude.InstanceRoot != "/instances/acme" {
		t.Fatalf("adapter instance root = %q, want /instances/acme", claude.InstanceRoot)
	}
	if claude.SelfBin != "/opt/goobers/bin/goobers" {
		t.Fatalf("adapter self binary = %q, want /opt/goobers/bin/goobers", claude.SelfBin)
	}
}

// runValidate and daemon startup both call compiledMachines, so this golden
// list is the automated-check contract for every surviving config admission
// path. A registry change must update this list rather than silently drifting.
func TestValidationAutomatedChecksGolden(t *testing.T) {
	want := []string{
		"ci-status",
		"land-outcome",
		"output-equals",
		"output-matches",
		"output-not-equals",
		"output-numeric-gte",
		"output-numeric-lt",
		"output-numeric-lte",
		"queue-outcome",
		"status-equals",
	}
	if got := knownAutomatedCheckNames(); !slices.Equal(got, want) {
		t.Fatalf("validation automated checks = %v, want %v", got, want)
	}
}

func TestCompiledMachinesRejectsInvalidGooberRuntimeConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec apiv1.GooberSpec
		want string
	}{
		{
			name: "unknown model",
			spec: apiv1.GooberSpec{Harness: apiv1.HarnessCopilot, Model: "unknown-model"},
			want: `unknown model "unknown-model"; valid models:`,
		},
		{
			name: "unknown option",
			spec: apiv1.GooberSpec{
				Harness: apiv1.HarnessCopilot,
				HarnessOptions: map[string]apiextensionsv1.JSON{
					"temperature": {Raw: []byte(`"0.2"`)},
				},
			},
			want: `unknown harness option "temperature"`,
		},
		{
			name: "unsupported model option",
			spec: apiv1.GooberSpec{
				Harness: apiv1.HarnessCopilot,
				Model:   "claude-sonnet-4.5",
				HarnessOptions: map[string]apiextensionsv1.JSON{
					"reasoningEffort": {Raw: []byte(`"high"`)},
				},
			},
			want: `reasoningEffort value "high" is not supported by model "claude-sonnet-4.5"`,
		},
		{
			name: "undeclared MCP credential",
			spec: apiv1.GooberSpec{
				MCPServers: []apiv1.MCPServer{{
					Name: "context",
					URL:  "https://mcp.example.test",
					CredentialRefs: []apiv1.MCPCredentialRef{{
						Capability: "contents:read",
						Header:     "Authorization",
					}},
				}},
			},
			want: `capability "contents:read" is not declared`,
		},
		{
			name: "unsupported MCP harness",
			spec: apiv1.GooberSpec{
				Harness: apiv1.HarnessClaudeCode,
				MCPServers: []apiv1.MCPServer{{
					Name:    "context",
					Command: "context-server",
				}},
			},
			want: `mcpServers are only supported by harness "copilot"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := compiledMachinesWithWarnings(
				&instance.ConfigSet{},
				map[string]apiv1.GooberSpec{"coder": tc.spec},
				nil,
			)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("compiledMachinesWithWarnings error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCompiledMachinesWarnsAndAdmitsModelFallback(t *testing.T) {
	_, resolvedGoobers, warnings, err := compiledMachinesWithWarnings(
		&instance.ConfigSet{},
		map[string]apiv1.GooberSpec{
			"coder": {
				Harness: apiv1.HarnessCopilot,
				Model:   "retired-model",
				HarnessOptions: map[string]apiextensionsv1.JSON{
					"fallback-to-default": {Raw: []byte("true")},
				},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("compiledMachinesWithWarnings: %v", err)
	}
	if len(warnings) != 1 ||
		warnings[0].Goober != "coder" ||
		warnings[0].Warning.Kind != harness.ConfigWarningModelFallback ||
		!strings.Contains(warnings[0].Warning.Message, `"retired-model"`) {
		t.Fatalf("warnings = %+v, want one coder model-fallback warning", warnings)
	}
	if resolvedGoobers["coder"].Model != "" ||
		len(resolvedGoobers["coder"].HarnessOptions) != 0 {
		t.Fatalf("resolved goober = %+v, want harness default without admission-only options", resolvedGoobers["coder"])
	}
}

func TestCompiledMachinesCarriesResolutionAndHarnessEnvironmentToExecutor(t *testing.T) {
	copilotHome := t.TempDir()
	t.Setenv("COPILOT_HOME", copilotHome)
	lister := &runnerWiringModelLister{responses: [][]harness.CopilotModelInfo{
		{{ID: "gpt-5.4"}},
	}}
	previousLister := copilotModelLister
	copilotModelLister = lister
	t.Cleanup(func() { copilotModelLister = previousLister })
	var runRequest harness.RunRequest
	previousAdapter := newAgenticAdapter
	newAgenticAdapter = func(string, map[string]string) harness.Adapter {
		return &harness.FakeAdapter{Act: func(_ context.Context, req harness.RunRequest) error {
			runRequest = req
			return harness.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{
				Status: apiv1.ResultSuccess,
			})
		}}
	}
	t.Cleanup(func() { newAgenticAdapter = previousAdapter })

	_, resolvedGoobers, warnings, err := compiledMachinesWithWarnings(
		&instance.ConfigSet{},
		map[string]apiv1.GooberSpec{
			"coder": {
				Harness: apiv1.HarnessCopilot,
				Model:   "retired-model",
				HarnessOptions: map[string]apiextensionsv1.JSON{
					"fallback-to-default": {Raw: []byte("true")},
				},
			},
		},
		[]string{"COPILOT_HOME"},
	)
	if err != nil {
		t.Fatalf("compiledMachinesWithWarnings: %v", err)
	}
	if len(warnings) != 1 || resolvedGoobers["coder"].Model != "" {
		t.Fatalf("admission = goober %+v warnings %+v, want fallback to default", resolvedGoobers["coder"], warnings)
	}
	if !slices.Contains(lister.env, "COPILOT_HOME="+copilotHome) {
		t.Fatalf("model discovery env = %v, want configured COPILOT_HOME", lister.env)
	}

	layout := instance.NewLayout(t.TempDir())
	runnerCfg, _, err := buildRunnerConfig(
		layout,
		&instance.Config{Runner: instance.RunnerConfig{EnvPassthrough: []string{"COPILOT_HOME"}}},
		resolvedGoobers,
		map[string]string{"coder": "instructions"},
		nil,
		journal.NewRegistryScrubber(),
		nil,
		nil,
		apiv1.RepoRef{},
		nil,
		nil,
		nil,
		instance.SandboxDisabled,
	)
	if err != nil {
		t.Fatalf("buildRunnerConfig: %v", err)
	}
	recorder := runnerWiringHarnessRecorder{dir: t.TempDir()}
	agentic, err := runnerCfg.NewAgentic("coder", recorder, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatalf("NewAgentic: %v", err)
	}
	if _, err := agentic.Invoke(context.Background(), apiv1.InvocationEnvelope{
		TaskID:    "implement",
		Workspace: recorder.dir,
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !runRequest.HarnessConfigResolved || runRequest.Model != "" ||
		len(runRequest.HarnessOptions) != 0 {
		t.Fatalf("runtime request = %+v, want admitted harness default", runRequest)
	}
	if lister.calls != 1 {
		t.Fatalf("model discovery calls = %d, want admission query only", lister.calls)
	}
}

func TestBuildRunnerConfigRejectsMCPServersForUnsupportedHarness(t *testing.T) {
	const gooberName = "coder"
	spec := apiv1.GooberSpec{
		Harness: apiv1.HarnessClaudeCode,
		MCPServers: []apiv1.MCPServer{{
			Name:    "context",
			Command: "context-server",
		}},
	}
	cfg, _, err := buildRunnerConfig(
		instance.NewLayout(t.TempDir()),
		&instance.Config{},
		map[string]apiv1.GooberSpec{gooberName: spec},
		map[string]string{gooberName: "instructions"},
		nil,
		journal.NewRegistryScrubber(),
		nil,
		nil,
		apiv1.RepoRef{},
		nil,
		nil,
		nil,
		instance.SandboxDisabled,
	)
	if err != nil {
		t.Fatalf("buildRunnerConfig: %v", err)
	}

	_, err = cfg.NewAgentic(gooberName, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `mcpServers are only supported by harness "copilot"`) {
		t.Fatalf("NewAgentic error = %v, want unsupported-harness error", err)
	}
}

// TestBuildCredentialsDefault: with no credentials: block, the first repo's
// token backs every credentialed capability and agent:model is absent (it must
// be sourced explicitly, never defaulted to the repo token).
func TestBuildCredentialsDefault(t *testing.T) {
	t.Setenv("GH_TOKEN_A", "tokenA")
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "GH_TOKEN_A"}},
	}}
	resolver, grants, err := buildCredentials(cfg, nil, "", "", nil, nil)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}

	got := resolveGrants(t, resolver, grants)
	for _, c := range credentialedCapabilities {
		if got[string(c)] != "tokenA" {
			t.Fatalf("capability %s = %q, want repo token tokenA", c, got[string(c)])
		}
	}
	if _, ok := got["agent:model"]; ok {
		t.Fatalf("agent:model must not be granted without a credentials: entry, got %+v", got)
	}
	if _, ok := got[string(capability.ConfigRepoRead)]; ok {
		t.Fatalf("configrepo:read must not default to the repo token, got %+v", got)
	}
}

func TestStageCredentialsCannotObtainWorkflowSourceToken(t *testing.T) {
	t.Setenv("CODE_REPO_TOKEN", "code-repo-token")
	t.Setenv("WORKFLOW_SOURCE_TOKEN", "workflow-source-token")
	cfg := &instance.Config{
		Repos: []instance.RepoRef{{
			Provider: "github",
			Owner:    "acme",
			Name:     "web",
			Token:    instance.TokenRef{Env: "CODE_REPO_TOKEN"},
		}},
		WorkflowSource: &instance.WorkflowSource{
			Kind:  instance.WorkflowSourceKindGit,
			URL:   "https://example.com/workflow-config.git",
			Token: &instance.TokenRef{Env: "WORKFLOW_SOURCE_TOKEN"},
		},
	}
	resolver, grants, err := buildCredentials(cfg, nil, "", "", nil, nil)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, grants, &escTestRegistrar{})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	set, err := injector.Materialize(context.Background(), []string{string(capability.ConfigRepoRead)})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if _, err := set.Token(context.Background(), string(capability.ConfigRepoRead)); !errors.Is(err, credentials.ErrNoCredentialForCapability) {
		t.Fatalf("stage configrepo:read token error = %v, want ErrNoCredentialForCapability", err)
	}
}

func TestBuildCredentialsRejectsRunnerOnlyOverride(t *testing.T) {
	cfg := &instance.Config{Credentials: []instance.CredentialGrant{{
		Capability: string(capability.ConfigRepoRead),
		Token:      instance.TokenRef{Env: "WORKFLOW_SOURCE_TOKEN"},
	}}}
	if _, _, err := buildCredentials(cfg, nil, "", "", nil, nil); err == nil ||
		!strings.Contains(err.Error(), `"configrepo:read" cannot be stage-scoped`) {
		t.Fatalf("buildCredentials error = %v, want runner-only override rejection", err)
	}
}

func TestBuildCredentialsScopesBYOMCPGrantToReferencingGoober(t *testing.T) {
	t.Setenv("SHAREPOINT_MCP_TOKEN", "sharepoint-secret")
	cfg := &instance.Config{Credentials: []instance.CredentialGrant{{
		MCP:   "sharepoint",
		Token: instance.TokenRef{Env: "SHAREPOINT_MCP_TOKEN"},
	}}}
	resolver, grants, err := buildCredentials(cfg, nil, "", "", nil, nil)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	key := mcpconfig.BYOCredentialKey("sharepoint")
	gooberGrants := buildGooberCredentialGrants("knowledge", []string{key}, grants)
	injector, err := credentials.NewGooberInjectorWithCredentialKeys(
		resolver,
		"knowledge",
		gooberGrants,
		[]string{key},
		&escTestRegistrar{},
	)
	if err != nil {
		t.Fatalf("NewGooberInjectorWithCredentialKeys: %v", err)
	}
	set, err := injector.Materialize(context.Background(), nil)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	token, err := set.Token(context.Background(), key)
	if err != nil || token != "sharepoint-secret" {
		t.Fatalf("BYO MCP token = %q, %v", token, err)
	}

	otherGrants := buildGooberCredentialGrants("coder", nil, grants)
	other, err := credentials.NewGooberInjector(resolver, "coder", otherGrants, &escTestRegistrar{})
	if err != nil {
		t.Fatalf("NewGooberInjector(other): %v", err)
	}
	otherSet, err := other.Materialize(context.Background(), []string{key})
	if err != nil {
		t.Fatalf("Materialize(other): %v", err)
	}
	if _, err := otherSet.Token(context.Background(), key); !errors.Is(err, credentials.ErrNoCredentialForCapability) {
		t.Fatalf("unreferencing goober token error = %v, want ErrNoCredentialForCapability", err)
	}
}

func TestDeterministicCredentialsRejectForgedBYOMCPEnvelope(t *testing.T) {
	t.Setenv("SHAREPOINT_MCP_TOKEN", "sharepoint-secret")
	t.Setenv("PUSH_TOKEN", "push-secret")
	cfg := &instance.Config{Credentials: []instance.CredentialGrant{
		{MCP: "sharepoint", Token: instance.TokenRef{Env: "SHAREPOINT_MCP_TOKEN"}},
		{Capability: string(capability.RepoPush), Token: instance.TokenRef{Env: "PUSH_TOKEN"}},
	}}
	resolver, sources, err := buildCredentials(cfg, nil, "", "", nil, nil)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, deterministicCredentialGrants(sources), &escTestRegistrar{})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	key := mcpconfig.BYOCredentialKey("sharepoint")
	forged := apiv1.InvocationEnvelope{
		Capabilities: []string{key, string(capability.RepoPush)},
	}
	set, err := injector.Materialize(context.Background(), forged.Capabilities)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if _, err := set.Token(context.Background(), key); !errors.Is(err, credentials.ErrNoCredentialForCapability) {
		t.Fatalf("forged deterministic BYO MCP token error = %v, want ErrNoCredentialForCapability", err)
	}
	token, err := set.Token(context.Background(), string(capability.RepoPush))
	if err != nil || token != "push-secret" {
		t.Fatalf("first-party deterministic token = %q, %v, want push-secret", token, err)
	}
}

// TestBuildCredentialsStoreBackedRepoToken pins #683 at the main composition
// root: a store-backed repo token backs the credentialed capabilities exactly
// like an env/file token, the value reaches the injector's registrar (the
// journal-scrubber seam), and a missing store registry fails construction
// closed instead of leaving the repo silently tokenless.
func TestBuildCredentialsStoreBackedRepoToken(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Store: "prod-kv/github-token"}},
	}}
	resolver, grants, err := buildCredentials(cfg, wiringFakeStoreResolver{"prod-kv/github-token": "kv-repo-token"}, "", "", nil, nil)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	registrar := &escTestRegistrar{}
	injector, err := credentials.NewInjector(resolver, grants, registrar)
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	set, err := injector.Materialize(context.Background(), []string{string(capability.RepoPush)})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	token, err := set.Token(context.Background(), string(capability.RepoPush))
	if err != nil || token != "kv-repo-token" {
		t.Fatalf("repo:push token = (%q, %v), want the store-resolved token", token, err)
	}
	if len(registrar.registered) != 1 || string(registrar.registered[0]) != "kv-repo-token" {
		t.Fatalf("scrubber registrations = %q, want exactly the store-resolved token", registrar.registered)
	}

	if _, _, err := buildCredentials(cfg, nil, "", "", nil, nil); err == nil {
		t.Fatal("buildCredentials: want fail-closed error for store ref without a store registry, got nil")
	}
}

func TestBuildCredentialsAllowsTokenlessADOIdentity(t *testing.T) {
	t.Setenv("GH_TOKEN", "must-not-cross-gaggle-boundary")
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "other", Name: "repo", Token: instance.TokenRef{Env: "GH_TOKEN"}},
		{
			Provider: "ado",
			Owner:    "acme",
			Project:  "widgets",
			Name:     "web",
			Auth:     &instance.RepoAuthConfig{Kind: instance.ADOAuthAzureCLI},
		},
	}}
	_, grants, err := buildCredentials(cfg, nil, "acme/widgets", "web", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Fatalf("tokenless ADO grants = %#v, want none", grants)
	}
}

func TestBuildCredentialsScopesADOPATByProject(t *testing.T) {
	t.Setenv("ADO_PAT", "ado-token")
	cfg := &instance.Config{Repos: []instance.RepoRef{{
		Provider: "ado",
		Owner:    "acme",
		Project:  "widgets",
		Name:     "web",
		Token:    instance.TokenRef{Env: "ADO_PAT"},
	}}}
	resolver, grants, err := buildCredentials(cfg, nil, "acme/widgets", "web", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := resolveGrants(t, resolver, grants)
	if got[string(capability.RepoPush)] != "ado-token" {
		t.Fatalf("repo:push = %q", got[string(capability.RepoPush)])
	}
}

// TestBuildCredentialsGrantsReadOnlyAdditionalRepos proves the MGV-10 (#1285)
// end-to-end wiring: a gaggle whose Project is example/site (read-write) and
// whose AdditionalRepos include example/goobers (read-only) is granted the
// project's own write token plus a repo-qualified contents:read token for the
// reference repo — and no write capability against the reference repo.
func TestBuildCredentialsGrantsReadOnlyAdditionalRepos(t *testing.T) {
	t.Setenv("SITE_TOKEN", "site-write")
	t.Setenv("REF_TOKEN", "ref-read")
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "example", Name: "site", Token: instance.TokenRef{Env: "SITE_TOKEN"}},
		{Provider: "github", Owner: "example", Name: "goobers", Token: instance.TokenRef{Env: "REF_TOKEN"}},
	}}
	additional := []apiv1.RepoRef{{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "goobers"}}

	resolver, grants, err := buildCredentials(cfg, nil, "example", "site", additional, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := resolveGrants(t, resolver, grants)

	// The project's write capabilities resolve to the site (write) token.
	if got[string(capability.RepoPush)] != "site-write" {
		t.Errorf("repo:push = %q, want site-write", got[string(capability.RepoPush)])
	}
	// The reference repo's read capability resolves to its own read token.
	readCap := credentials.RepoScopedCapability(string(capability.ContentsRead), "example", "goobers")
	if got[readCap] != "ref-read" {
		t.Errorf("%s = %q, want ref-read", readCap, got[readCap])
	}
	// No capability targeting the reference repo is anything but read.
	for cap := range got {
		if strings.HasSuffix(cap, "@example/goobers") && !strings.HasPrefix(cap, string(capability.ContentsRead)+"@") {
			t.Errorf("reference repo example/goobers got a non-read grant %q", cap)
		}
	}
}

func TestADORepoForGaggleMatchesExplicitProject(t *testing.T) {
	want := instance.RepoRef{
		Provider: "ado",
		Owner:    "acme",
		Project:  "widgets",
		Name:     "web",
		Auth:     &instance.RepoAuthConfig{Kind: instance.ADOAuthAzureCLI},
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{want}}
	got, ok := adoRepoForGaggle(cfg, apiv1.RepoRef{
		Provider: apiv1.ProviderADO,
		Owner:    "acme/widgets",
		Name:     "web",
	})
	if !ok || got.Owner != want.Owner || got.Project != want.Project || got.Name != want.Name {
		t.Fatalf("adoRepoForGaggle() = %#v, %v", got, ok)
	}
}

func TestGitHubRepoForGaggle(t *testing.T) {
	want := instance.RepoRef{
		Provider: "github",
		Owner:    "acme",
		Name:     "web",
		Token:    instance.TokenRef{Env: "ACME_WEB_TOKEN"},
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "other"},
		want,
	}}
	got, ok := githubRepoForGaggle(cfg, apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"})
	if !ok || got.Owner != want.Owner || got.Name != want.Name || got.Token.Env != want.Token.Env {
		t.Fatalf("githubRepoForGaggle() = %#v, %v", got, ok)
	}

	// A legacy single-repo instance with an unqualified project falls back to
	// the lone configured repo, mirroring adoRepoForGaggle.
	single := &instance.Config{Repos: []instance.RepoRef{want}}
	if got, ok := githubRepoForGaggle(single, apiv1.RepoRef{}); !ok || got.Name != want.Name {
		t.Fatalf("githubRepoForGaggle(single-repo fallback) = %#v, %v", got, ok)
	}

	if got, ok := githubRepoForGaggle(cfg, apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "acme", Name: "web"}); ok {
		t.Fatalf("githubRepoForGaggle(ado project) = %#v, want no match", got)
	}
}

// --- #667: authenticated GitHub mirror clone/fetch ---

func TestGitHubWorktreeGitEnvironmentNoTokenIsNoOp(t *testing.T) {
	workcopies := t.TempDir()
	resolve, err := githubWorktreeGitEnvironment(workcopies, instance.RepoRef{
		Provider: "github", Owner: "acme", Name: "web",
	}, nil, nil)
	if err != nil {
		t.Fatalf("githubWorktreeGitEnvironment: %v", err)
	}
	if resolve != nil {
		t.Fatal("token-less repo produced a git-environment resolver, want nil (public-repo path unchanged)")
	}
	if _, err := os.Stat(filepath.Join(workcopies, "auth")); !os.IsNotExist(err) {
		t.Fatalf("askpass dir written for a token-less repo (stat err = %v)", err)
	}
}

// TestGitHubWorktreeGitEnvironmentStoreBackedToken pins the #683 half of the
// #667 seam: a store-backed repo token is a configured token — it must
// authenticate clone/fetch (never silently fall into the public-repo nil-
// resolver arm) and must fail closed when no store resolver is wired.
func TestGitHubWorktreeGitEnvironmentStoreBackedToken(t *testing.T) {
	repo := instance.RepoRef{
		Provider: "github", Owner: "acme", Name: "web",
		Token: instance.TokenRef{Store: "prod-kv/github-token"},
	}
	registrar := &escTestRegistrar{}
	resolve, err := githubWorktreeGitEnvironment(t.TempDir(), repo, registrar,
		wiringFakeStoreResolver{"prod-kv/github-token": "kv-gh-token"})
	if err != nil {
		t.Fatalf("githubWorktreeGitEnvironment: %v", err)
	}
	if resolve == nil {
		t.Fatal("store-backed token produced no git-environment resolver (would clone unauthenticated)")
	}
	env, err := resolve(context.Background(), "https://github.com/acme/web.git")
	if err != nil {
		t.Fatalf("resolve configured repo: %v", err)
	}
	found := false
	for _, entry := range env {
		if entry == "GOOBERS_GIT_TOKEN=kv-gh-token" {
			found = true
		}
	}
	if !found {
		t.Fatalf("GOOBERS_GIT_TOKEN missing store-resolved token: %q", env)
	}
	if len(registrar.registered) != 1 || string(registrar.registered[0]) != "kv-gh-token" {
		t.Fatalf("scrubber registrations = %q, want exactly the store-resolved token", registrar.registered)
	}

	if _, err := githubWorktreeGitEnvironment(t.TempDir(), repo, registrar, nil); err == nil {
		t.Fatal("githubWorktreeGitEnvironment: want fail-closed error for store ref without a store resolver, got nil")
	}
}

func TestGitHubWorktreeGitEnvironmentInjectsAskpassForConfiguredRepoOnly(t *testing.T) {
	t.Setenv("GOOBERS_TEST_667_TOKEN", "gh-token-value")
	workcopies := t.TempDir()
	registrar := &escTestRegistrar{}
	resolve, err := githubWorktreeGitEnvironment(workcopies, instance.RepoRef{
		Provider: "github", Owner: "acme", Name: "web",
		Token: instance.TokenRef{Env: "GOOBERS_TEST_667_TOKEN"},
	}, registrar, nil)
	if err != nil {
		t.Fatalf("githubWorktreeGitEnvironment: %v", err)
	}
	if resolve == nil {
		t.Fatal("configured token produced no git-environment resolver")
	}

	env, err := resolve(context.Background(), "https://github.com/acme/web.git")
	if err != nil {
		t.Fatalf("resolve configured repo: %v", err)
	}
	values := map[string]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	askpass := values["GIT_ASKPASS"]
	if askpass == "" {
		t.Fatalf("GIT_ASKPASS missing from environment: %q", env)
	}
	script, err := os.ReadFile(askpass)
	if err != nil {
		t.Fatalf("read askpass script: %v", err)
	}
	if strings.Contains(string(script), "gh-token-value") {
		t.Fatal("askpass script on disk contains the token")
	}
	if got := values["GOOBERS_GIT_TOKEN"]; got != "gh-token-value" {
		t.Fatalf("GOOBERS_GIT_TOKEN = %q, want the configured token", got)
	}
	if got := values["GIT_TERMINAL_PROMPT"]; got != "0" {
		t.Fatalf("GIT_TERMINAL_PROMPT = %q, want 0 (fail closed, no interactive hang)", got)
	}
	if len(registrar.registered) != 1 || string(registrar.registered[0]) != "gh-token-value" {
		t.Fatalf("scrubber registrations = %q, want exactly the resolved token", registrar.registered)
	}

	// .git-less spelling of the same repo still authenticates.
	if env, err := resolve(context.Background(), "https://github.com/acme/web"); err != nil || env == nil {
		t.Fatalf("resolve .git-less URL = (%v, %v), want authenticated environment", env, err)
	}
	// Any other remote gets the ambient, unauthenticated environment: the
	// token is scoped to the configured repo.
	if env, err := resolve(context.Background(), "https://github.com/acme/other.git"); err != nil || env != nil {
		t.Fatalf("resolve foreign URL = (%v, %v), want (nil, nil)", env, err)
	}
}

func TestGitHubWorktreeGitEnvironmentFailsClosedOnUnresolvableToken(t *testing.T) {
	// Set but empty: TokenRef resolution treats an empty value as
	// misconfiguration, and setting it makes the test hermetic regardless of
	// the ambient environment.
	t.Setenv("GOOBERS_TEST_667_EMPTY_TOKEN", "")
	resolve, err := githubWorktreeGitEnvironment(t.TempDir(), instance.RepoRef{
		Provider: "github", Owner: "acme", Name: "web",
		Token: instance.TokenRef{Env: "GOOBERS_TEST_667_EMPTY_TOKEN"},
	}, nil, nil)
	if err != nil {
		t.Fatalf("githubWorktreeGitEnvironment: %v", err)
	}
	if _, err := resolve(context.Background(), "https://github.com/acme/web.git"); err == nil {
		t.Fatal("unresolvable token ref must fail provisioning closed, got nil error")
	}
}

// --- #686: GitHub App installation-token minting ---

// TestBuildCredentialsGitHubAppMintsRepoToken: a github-app repo's ref
// resolves through the minting source under the same owner/name ref a static
// token would use, so every capability grant receives installation tokens —
// and re-resolution reaches the source again (its cache, not the resolver,
// decides freshness).
func TestBuildCredentialsGitHubAppMintsRepoToken(t *testing.T) {
	prev := newGitHubAppTokenSource
	mints := 0
	var gotRepo instance.RepoRef
	newGitHubAppTokenSource = func(repo instance.RepoRef, _ credentials.SecretRegistrar, _ credentials.StoreResolver) (credentials.ResolveFunc, error) {
		gotRepo = repo
		return func(context.Context) (string, error) {
			mints++
			return fmt.Sprintf("minted-token-%d", mints), nil
		}, nil
	}
	t.Cleanup(func() { newGitHubAppTokenSource = prev })

	cfg := &instance.Config{Repos: []instance.RepoRef{{
		Provider: "github", Owner: "acme", Name: "web",
		Auth: &instance.RepoAuthConfig{
			Kind: instance.GitHubAuthApp, AppID: "123456", InstallationID: "42",
			PrivateKey: &instance.TokenRef{File: "/run/secrets/app.pem"},
		},
	}}}
	resolver, grants, err := buildCredentials(cfg, nil, "", "", nil, nil)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	if gotRepo.Owner != "acme" || gotRepo.Name != "web" || !gotRepo.GitHubAppAuth() {
		t.Fatalf("minting source built for %+v, want the github-app repo", gotRepo)
	}
	got := resolveGrants(t, resolver, grants)
	for _, c := range credentialedCapabilities {
		if !strings.HasPrefix(got[string(c)], "minted-token-") {
			t.Fatalf("capability %s = %q, want a minted installation token", c, got[string(c)])
		}
	}
	if _, err := resolver.Resolve(context.Background(), "acme/web"); err != nil {
		t.Fatalf("Resolve repo ref: %v", err)
	}
	if mints < 2 {
		t.Fatalf("mints = %d, want the source consulted per resolve", mints)
	}
}

func TestBuildCredentialsGitHubAppSourceFailureFailsClosed(t *testing.T) {
	prev := newGitHubAppTokenSource
	newGitHubAppTokenSource = func(instance.RepoRef, credentials.SecretRegistrar, credentials.StoreResolver) (credentials.ResolveFunc, error) {
		return nil, errors.New("store-backed key not resolvable here")
	}
	t.Cleanup(func() { newGitHubAppTokenSource = prev })

	cfg := &instance.Config{Repos: []instance.RepoRef{{
		Provider: "github", Owner: "acme", Name: "web",
		Auth: &instance.RepoAuthConfig{
			Kind: instance.GitHubAuthApp, AppID: "123456", InstallationID: "42",
			PrivateKey: &instance.TokenRef{Store: "kv/app-key"},
		},
	}}}
	if _, _, err := buildCredentials(cfg, nil, "", "", nil, nil); err == nil ||
		!strings.Contains(err.Error(), "repo acme/web") {
		t.Fatalf("buildCredentials error = %v, want fail-closed repo diagnosis", err)
	}
}

// TestBuildCredentialsDuplicateGitHubAppReposFailClosed: two github-app repos
// with the same owner/name must be rejected at build time (as a duplicate
// static-token ref already is at NewResolverWith), never silently collapse to
// the last entry's minting source and hand it the first's grants.
func TestBuildCredentialsDuplicateGitHubAppReposFailClosed(t *testing.T) {
	prev := newGitHubAppTokenSource
	newGitHubAppTokenSource = func(instance.RepoRef, credentials.SecretRegistrar, credentials.StoreResolver) (credentials.ResolveFunc, error) {
		return func(context.Context) (string, error) { return "minted", nil }, nil
	}
	t.Cleanup(func() { newGitHubAppTokenSource = prev })

	appRepo := func() instance.RepoRef {
		return instance.RepoRef{
			Provider: "github", Owner: "acme", Name: "web",
			Auth: &instance.RepoAuthConfig{
				Kind: instance.GitHubAuthApp, AppID: "123456", InstallationID: "42",
				PrivateKey: &instance.TokenRef{File: "/run/secrets/app.pem"},
			},
		}
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{appRepo(), appRepo()}}
	if _, _, err := buildCredentials(cfg, nil, "", "", nil, nil); err == nil ||
		!strings.Contains(err.Error(), "duplicate repository reference") {
		t.Fatalf("buildCredentials error = %v, want duplicate-repo fail-closed", err)
	}
}

// TestGitHubWorktreeGitEnvironmentMintsForGitHubAppRepo: a github-app repo's
// mirror clone/fetch environment mints per operation, so a refreshed
// installation token reaches the next fetch without re-wiring (#667 + #686).
func TestGitHubWorktreeGitEnvironmentMintsForGitHubAppRepo(t *testing.T) {
	prev := newGitHubAppTokenSource
	mints := 0
	newGitHubAppTokenSource = func(repo instance.RepoRef, _ credentials.SecretRegistrar, _ credentials.StoreResolver) (credentials.ResolveFunc, error) {
		return func(context.Context) (string, error) {
			mints++
			return fmt.Sprintf("ghs_minted_%d", mints), nil
		}, nil
	}
	t.Cleanup(func() { newGitHubAppTokenSource = prev })

	registrar := &escTestRegistrar{}
	resolve, err := githubWorktreeGitEnvironment(t.TempDir(), instance.RepoRef{
		Provider: "github", Owner: "acme", Name: "web",
		Auth: &instance.RepoAuthConfig{
			Kind: instance.GitHubAuthApp, AppID: "123456", InstallationID: "42",
			PrivateKey: &instance.TokenRef{File: "/run/secrets/app.pem"},
		},
	}, registrar, nil)
	if err != nil {
		t.Fatalf("githubWorktreeGitEnvironment: %v", err)
	}
	if resolve == nil {
		t.Fatal("github-app repo produced no git-environment resolver")
	}
	env, err := resolve(context.Background(), "https://github.com/acme/web.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var token string
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "GOOBERS_GIT_TOKEN="); ok {
			token = value
		}
	}
	if token != "ghs_minted_1" {
		t.Fatalf("GOOBERS_GIT_TOKEN = %q, want the minted token", token)
	}
	if len(registrar.registered) != 1 || string(registrar.registered[0]) != "ghs_minted_1" {
		t.Fatalf("scrubber registrations = %q, want the minted token", registrar.registered)
	}
	// The next operation re-resolves: a refreshed token flows in transparently.
	if _, err := resolve(context.Background(), "https://github.com/acme/web.git"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if mints != 2 {
		t.Fatalf("mints = %d, want per-operation resolution", mints)
	}
	// Foreign remotes stay unauthenticated — no mint spent on them.
	if env, err := resolve(context.Background(), "https://github.com/acme/other.git"); err != nil || env != nil {
		t.Fatalf("resolve foreign URL = (%v, %v), want (nil, nil)", env, err)
	}
	if mints != 2 {
		t.Fatalf("mints = %d after foreign URL, want no extra mint", mints)
	}
}

// TestGitAuthEnvironmentSupportsMirrorCloneAndFetch proves the injected
// environment is a complete, working child environment for real git: both the
// initial mirror clone and the subsequent refresh fetch run under exactly the
// environment githubWorktreeGitEnvironment injects (a local origin stands in
// for github.com — path-based access never prompts, so the run also shows the
// askpass wiring is inert when the remote demands no credential).
func TestGitAuthEnvironmentSupportsMirrorCloneAndFetch(t *testing.T) {
	origin := initBareOrigin(t)
	askpass, err := credentials.WriteAskpassScript(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatalf("WriteAskpassScript: %v", err)
	}
	calls := 0
	manager, err := worktree.NewManager(t.TempDir(), worktree.WithGitEnvironment(func(context.Context, string) ([]string, error) {
		calls++
		return credentials.GitAuthEnvironment(askpass, "test-token"), nil
	}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.WorkingCopy(context.Background(), origin); err != nil {
		t.Fatalf("mirror clone under authenticated environment: %v", err)
	}
	if _, err := manager.WorkingCopy(context.Background(), origin); err != nil {
		t.Fatalf("refresh fetch under authenticated environment: %v", err)
	}
	if calls != 2 {
		t.Fatalf("git environment resolutions = %d, want 2 (clone + fetch)", calls)
	}
}

func TestPathLengthManagerOptions(t *testing.T) {
	cloneURL := func(repo apiv1.RepoRef) (string, error) {
		return fmt.Sprintf("https://example.test/%s/%s.git", repo.Owner, repo.Name), nil
	}
	base := instance.RepoRef{Provider: "github", Owner: "acme", Name: "web"}

	options, err := pathLengthManagerOptions(&instance.Config{Repos: []instance.RepoRef{base}}, cloneURL, "linux")
	if err != nil || len(options) != 0 {
		t.Fatalf("unconfigured linux options = %d, %v; want 0", len(options), err)
	}
	options, err = pathLengthManagerOptions(&instance.Config{Repos: []instance.RepoRef{base}}, cloneURL, "windows")
	if err != nil || len(options) != 1 {
		t.Fatalf("unconfigured windows options = %d, %v; want 1", len(options), err)
	}
	base.PathLength = &instance.RepoPathLengthConfig{MaxPathLength: 320, BuildOutputAllowance: 40}
	options, err = pathLengthManagerOptions(&instance.Config{Repos: []instance.RepoRef{base}}, cloneURL, "linux")
	if err != nil || len(options) != 1 {
		t.Fatalf("configured linux options = %d, %v; want 1", len(options), err)
	}
	base.PathLength.Disabled = true
	options, err = pathLengthManagerOptions(&instance.Config{Repos: []instance.RepoRef{base}}, cloneURL, "windows")
	if err != nil || len(options) != 0 {
		t.Fatalf("disabled windows options = %d, %v; want 0", len(options), err)
	}
}

func TestBuildRunnerConfigReloadsPathLengthPolicyOnReusedManager(t *testing.T) {
	previousCloneURL := repoCloneURL
	origin := initBareOrigin(t)
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return origin, nil }
	t.Cleanup(func() { repoCloneURL = previousCloneURL })

	layout := instance.NewLayout(t.TempDir())
	if err := layout.EnsureGaggleRuntime("example"); err != nil {
		t.Fatal(err)
	}
	layout = layout.ForGaggle("example")
	repo := instance.RepoRef{
		Provider:   "github",
		Owner:      "acme",
		Name:       "web",
		PathLength: &instance.RepoPathLengthConfig{Disabled: true},
	}
	build := func(manager *worktree.Manager) *worktree.Manager {
		t.Helper()
		_, manager, err := buildRunnerConfig(
			layout,
			&instance.Config{Repos: []instance.RepoRef{repo}},
			map[string]apiv1.GooberSpec{},
			map[string]string{},
			nil,
			journal.NewRegistryScrubber(),
			manager,
			nil,
			apiv1.RepoRef{},
			nil,
			harnessPreflightInfo{},
			nil,
			instance.SandboxDisabled,
		)
		if err != nil {
			t.Fatalf("buildRunnerConfig: %v", err)
		}
		return manager
	}

	manager := build(nil)
	repo.PathLength = &instance.RepoPathLengthConfig{MaxPathLength: 1}
	reused := build(manager)
	if reused != manager {
		t.Fatal("config reload replaced the worktree manager")
	}
	if _, err := manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: origin,
		RunID:   "enabled-after-reload",
		BaseRef: "main",
	}); err == nil {
		t.Fatal("Create succeeded after reload enabled an exhausted path budget")
	}

	repo.PathLength.Disabled = true
	build(manager)
	wt, err := manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: origin,
		RunID:   "disabled-after-reload",
		BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("Create after reload disabled path preflight: %v", err)
	}
	if err := wt.Remove(context.Background(), worktree.RemoveOptions{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestWorkflowRuntimeIndexesUseGaggleAndName(t *testing.T) {
	testBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workflowDefinition := func(gaggle, dslVersion string) apiv1.Workflow {
		return apiv1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "deploy"},
			DSLVersion: dslVersion,
			Spec: apiv1.WorkflowSpec{
				Gaggle:   gaggle,
				Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
				Start:    "deploy",
				Tasks: []apiv1.Task{{
					Name: "deploy",
					Type: apiv1.TaskDeterministic,
					Goal: "Deploy.",
					Run:  &apiv1.DeterministicRun{Command: []string{testBin, "-test.run=^$"}, Workspace: apiv1.WorkspaceScratch},
				}},
			},
		}
	}
	set := &instance.ConfigSet{
		Manifest: &apiv1.Manifest{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{workflow.PreviewFeaturesAnnotation: "true"},
		}},
		Gaggles: []apiv1.Gaggle{
			{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}, Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "alpha"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "beta"}, Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "beta"}}},
		},
		Workflows: []apiv1.Workflow{
			workflowDefinition("alpha", "1.4"),
			workflowDefinition("beta", ""),
		},
	}

	machines, _, _, err := compiledMachinesWithWarnings(set, map[string]apiv1.GooberSpec{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := repoRefsByWorkflow(set)
	if err != nil {
		t.Fatal(err)
	}
	alpha := localscheduler.WorkflowIdentity{Gaggle: "alpha", Workflow: "deploy"}
	beta := localscheduler.WorkflowIdentity{Gaggle: "beta", Workflow: "deploy"}
	if len(machines) != 2 || machines[alpha] == nil || machines[beta] == nil {
		t.Fatalf("compiled machines = %+v", machines)
	}
	if machines[alpha].Def.DSLVersion != "1.4" || machines[beta].Def.DSLVersion != "" {
		t.Fatalf("compiled machine DSL versions = alpha %q, beta %q",
			machines[alpha].Def.DSLVersion, machines[beta].Def.DSLVersion)
	}
	if len(refs) != 2 || refs[alpha].Name != "alpha" || refs[beta].Name != "beta" {
		t.Fatalf("workflow repo refs = %+v", refs)
	}

	layout := instance.NewLayout(t.TempDir())
	for _, gaggle := range []string{"alpha", "beta"} {
		if err := layout.EnsureGaggleRuntime(gaggle); err != nil {
			t.Fatal(err)
		}
	}
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	var wg sync.WaitGroup
	definitions, err := buildSchedulerDefinitions(
		layout,
		&instance.Config{},
		set,
		nil,
		&wg,
		newDaemonRunnerRegistry(),
		nil,
		nil,
		nil,
		log,
		journal.NewRegistryScrubber(),
		nil,
		localscheduler.NewProviderQuotaState(),
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if definitions.WorktreesByGaggle["alpha"].Root == definitions.WorktreesByGaggle["beta"].Root {
		t.Fatal("gaggles share a workcopy root")
	}
	for i, identity := range []localscheduler.WorkflowIdentity{alpha, beta} {
		runID, err := telemetry.NewRunID()
		if err != nil {
			t.Fatal(err)
		}
		result, err := definitions.Runners[identity.Gaggle].Start(context.Background(), runner.StartInput{
			RunID:   runID,
			Machine: definitions.Machines[identity],
			Gaggle:  identity.Gaggle,
		})
		if err != nil || result.Phase != journal.PhaseCompleted {
			t.Fatalf("start %s run %d: phase=%s err=%v", identity.Gaggle, i, result.Phase, err)
		}
		if _, err := os.Stat(filepath.Join(layout.ForGaggle(identity.Gaggle).RunsDir(), runID, "run.yaml")); err != nil {
			t.Fatalf("%s run journal: %v", identity.Gaggle, err)
		}
	}
}

func TestLegacyClaimNamespaceUsesOwningRunIdentity(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	providers := map[string]apiv1.Provider{
		"alpha": apiv1.ProviderGitHub,
		"beta":  apiv1.ProviderADO,
	}
	for _, test := range []struct {
		runID    string
		gaggle   string
		provider string
	}{
		{runID: "run-alpha", gaggle: "alpha", provider: "github"},
		{runID: "run-beta", gaggle: "beta", provider: "ado"},
	} {
		run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
			RunID:     test.runID,
			Workflow:  "deploy",
			Gaggle:    test.gaggle,
			StartedAt: time.Now(),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}

		namespace, err := legacyClaimNamespace(layout, providers, localscheduler.ClaimEntry{RunID: test.runID})
		if err != nil {
			t.Fatal(err)
		}
		if namespace.Gaggle != test.gaggle || namespace.Provider != test.provider {
			t.Fatalf("namespace for %s = %+v, want gaggle %q provider %q", test.runID, namespace, test.gaggle, test.provider)
		}
	}
}

func TestBuildSchedulerSetupMigratesLiveLegacyClaimForRemovedWorkflow(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	const runID = "removed-workflow-run"

	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "removed-workflow", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(layout.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("159", runID, "removed-workflow", time.Hour); err != nil || !ok {
		t.Fatalf("seed legacy claim: ok=%v err=%v", ok, err)
	}

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), layout, &wg)
	if err != nil {
		t.Fatalf("buildSchedulerSetup: %v", err)
	}
	defer setup.Shutdown(context.Background())

	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "159"}
	entry, ok := reopened.LookupScoped(key)
	if !ok || entry.RunID != runID {
		t.Fatalf("migrated claim = %+v, %v; want claim scoped from the run's gaggle", entry, ok)
	}
	if _, ok := reopened.Lookup("159"); ok {
		t.Fatal("item-only legacy claim remained after ownership was resolved without the removed workflow")
	}
}

func TestBuildSchedulerSetupJournalsLegacyRuntimeMigration(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: "legacy-run", Workflow: "default-implement", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	legacyWorkcopy := filepath.Join(layout.WorkcopiesDir(), "legacy-repo", "repo.git", "HEAD")
	if err := os.MkdirAll(filepath.Dir(legacyWorkcopy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyWorkcopy, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), layout, &wg)
	if err != nil {
		t.Fatalf("buildSchedulerSetup: %v", err)
	}
	defer setup.Shutdown(context.Background())

	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var notes []journal.Event
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["note"] == legacyRuntimeMigrationNote {
			notes = append(notes, event)
		}
	}
	if len(notes) != 1 {
		t.Fatalf("legacy runtime migration notes = %d, want 1: %+v", len(notes), events)
	}
	if notes[0].Runner["gaggle"] != "example" {
		t.Fatalf("migration gaggle = %v, want example", notes[0].Runner["gaggle"])
	}
	moved, ok := notes[0].Runner["movedDirectories"].([]any)
	if !ok || !slices.Equal(moved, []any{instance.RunsDirName, instance.WorkcopiesDirName}) {
		t.Fatalf("moved directories = %#v, want runs and workcopies", notes[0].Runner["movedDirectories"])
	}
	pending, err := layout.MigrateLegacyRuntimeWithReport([]string{"example"})
	if err != nil {
		t.Fatal(err)
	}
	if pending.ID != "" || len(pending.MovedDirs) != 0 {
		t.Fatalf("journaled migration remained pending: %+v", pending)
	}
}

func TestJournalLegacyRuntimeMigrationReconcilesAfterRestart(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	legacyWorkcopy := filepath.Join(layout.WorkcopiesDir(), "legacy-repo", "repo.git", "HEAD")
	if err := os.MkdirAll(filepath.Dir(legacyWorkcopy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyWorkcopy, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	migration, err := layout.MigrateLegacyRuntimeWithReport([]string{"example"})
	if err != nil {
		t.Fatal(err)
	}

	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := instanceLog.Close(); err != nil {
		t.Fatal(err)
	}
	if err := journalLegacyRuntimeMigration(layout, instanceLog, migration); err == nil {
		t.Fatal("journalLegacyRuntimeMigration with closed log succeeded")
	}

	recovered, err := layout.MigrateLegacyRuntimeWithReport([]string{"example"})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != migration.ID ||
		recovered.Gaggle != migration.Gaggle ||
		!slices.Equal(recovered.MovedDirs, migration.MovedDirs) {
		t.Fatalf("recovered migration = %+v, want %+v", recovered, migration)
	}

	instanceLog, _, err = journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := instanceLog.Close(); err != nil {
			t.Errorf("close instance log: %v", err)
		}
	})
	if err := instanceLog.Append(legacyRuntimeMigrationEvent(recovered)); err != nil {
		t.Fatal(err)
	}
	if err := journalLegacyRuntimeMigration(layout, instanceLog, recovered); err != nil {
		t.Fatal(err)
	}

	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	notes := 0
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation &&
			event.Runner["note"] == legacyRuntimeMigrationNote &&
			event.Runner["migrationId"] == recovered.ID {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("legacy runtime migration notes = %d, want 1: %+v", notes, events)
	}
	pending, err := layout.MigrateLegacyRuntimeWithReport([]string{"example"})
	if err != nil {
		t.Fatal(err)
	}
	if pending.ID != "" || len(pending.MovedDirs) != 0 {
		t.Fatalf("reconciled migration remained pending: %+v", pending)
	}
}

// TestBuildCredentialsAgentModel: a credentials: entry for agent:model adds a
// grant sourced from its own token, leaving the repo-backed capabilities intact
// — the two-tokens-one-subprocess case (#287).
func TestBuildCredentialsAgentModel(t *testing.T) {
	t.Setenv("GH_TOKEN_A", "tokenA")
	t.Setenv("COPILOT_PAT", "copilottok")
	cfg := &instance.Config{
		Repos: []instance.RepoRef{
			{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "GH_TOKEN_A"}},
		},
		Credentials: []instance.CredentialGrant{
			{Capability: "agent:model", Token: instance.TokenRef{Env: "COPILOT_PAT"}},
		},
	}
	resolver, grants, err := buildCredentials(cfg, nil, "", "", nil, nil)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	got := resolveGrants(t, resolver, grants)
	if got["agent:model"] != "copilottok" {
		t.Fatalf("agent:model = %q, want copilottok", got["agent:model"])
	}
	for _, c := range credentialedCapabilities {
		if got[string(c)] != "tokenA" {
			t.Fatalf("capability %s = %q, want repo token tokenA", c, got[string(c)])
		}
	}
}

// TestBuildCredentialsOverride is #287 AC1/AC3: a credentials: entry for a
// capability the repo token would otherwise back OVERRIDES that grant — it
// resolves to the entry's token, and it stays a single grant (not duplicated).
func TestBuildCredentialsOverride(t *testing.T) {
	t.Setenv("GH_TOKEN_A", "tokenA")
	t.Setenv("PUSH_TOKEN_B", "tokenB")
	cfg := &instance.Config{
		Repos: []instance.RepoRef{
			{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "GH_TOKEN_A"}},
		},
		Credentials: []instance.CredentialGrant{
			{Capability: "repo:push", Token: instance.TokenRef{Env: "PUSH_TOKEN_B"}},
		},
	}
	resolver, grants, err := buildCredentials(cfg, nil, "", "", nil, nil)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	got := resolveGrants(t, resolver, grants)
	if got["repo:push"] != "tokenB" {
		t.Fatalf("repo:push = %q, want override tokenB", got["repo:push"])
	}
	// The other repo-backed capabilities are untouched by the override.
	if got["github:issues:write"] != "tokenA" || got["github:pr:write"] != "tokenA" {
		t.Fatalf("non-overridden capabilities changed: %+v", got)
	}
}

func TestBuildCredentialsApprovalOverride(t *testing.T) {
	t.Setenv("GH_TOKEN_A", "tokenA")
	t.Setenv("APPROVAL_TOKEN_B", "tokenB")
	cfg := &instance.Config{
		Repos: []instance.RepoRef{
			{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "GH_TOKEN_A"}},
		},
		Credentials: []instance.CredentialGrant{
			{Capability: "github:issues:approve", Token: instance.TokenRef{Env: "APPROVAL_TOKEN_B"}},
		},
	}
	resolver, grants, err := buildCredentials(cfg, nil, "", "", nil, nil)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	got := resolveGrants(t, resolver, grants)
	if got["github:issues:approve"] != "tokenB" {
		t.Fatalf("github:issues:approve = %q, want tokenB", got["github:issues:approve"])
	}
	if got["github:issues:write"] != "tokenA" {
		t.Fatalf("github:issues:write = %q, want repo token tokenA", got["github:issues:write"])
	}
}

func TestBuildGooberCredentialGrantsScopesSourcesToIdentity(t *testing.T) {
	sources := []credentials.Grant{
		{Capability: "agent:model", Ref: "model-token"},
		{Capability: "github:issues:write", Ref: "issues-token"},
		{Capability: "configrepo:read", Ref: "workflow-source-token"},
	}
	grants := buildGooberCredentialGrants(
		"curator",
		[]string{"agent:model", "telemetry:read", "configrepo:read", "agent:model"},
		sources,
	)
	if len(grants) != 1 {
		t.Fatalf("grants = %+v, want one credential-backed grant", grants)
	}
	if got := grants[0]; got.Goober != "curator" || got.Capability != "agent:model" || got.Ref != "model-token" {
		t.Fatalf("grant = %+v, want curator/agent:model/model-token", got)
	}
}

// TestIngestRunTelemetryLogsForcedFailure is issue #246's third fix: a
// swallowed rollup-ingest error used to leave nothing but a bare `_ =` — no
// visible trace anywhere that the rollup silently fell behind. This forces
// IngestRun to fail (a closed *rollup.DB) and asserts the failure is visible
// in the instance log, not merely absorbed.
func TestIngestRunTelemetryLogsForcedFailure(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)

	db, err := rollup.Open(filepath.Join(root, "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Force IngestRun/IngestSchedulerLog to fail deterministically, without
	// relying on any particular on-disk run-directory shape.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	ingestRunTelemetry(nil, db, nil, l, "run-forced-failure", log)

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range events {
		if ev.Type == journal.EventError && ev.RunID == "run-forced-failure" && ev.Error != nil &&
			strings.Contains(ev.Error.Code, "telemetry_ingest") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a telemetry_ingest_* error event for run-forced-failure, got: %+v", events)
	}
}

// TestIngestRunTelemetryNilLogDoesNotPanic proves logIngestFailure's nil-log
// guard holds — ingestRunTelemetry is called from contexts (tests, a
// standalone db) where no instance log may be wired.
func TestIngestRunTelemetryNilLogDoesNotPanic(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	db, err := rollup.Open(filepath.Join(root, "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ingestRunTelemetry(nil, db, nil, l, "run-nil-log", nil)
}

// --- #312: escalation-notifier wiring ---

type escTestRegistrar struct{ registered [][]byte }

func (r *escTestRegistrar) Register(secret []byte) {
	r.registered = append(r.registered, append([]byte(nil), secret...))
}

type ciPollTestRecorder struct{}

func (ciPollTestRecorder) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	return journal.Ref{Path: name, Digest: journal.Digest(data), Size: int64(len(data))}, nil
}

type ciPollFakePoller struct{ called bool }

func (p *ciPollFakePoller) PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	p.called = true
	return providers.PullRequestPollResult{CheckState: providers.CheckStatePassing}, nil
}

func newCIPollWiringTestExecutor(t *testing.T, reg *escTestRegistrar) invoke.Deterministic {
	t.Helper()
	t.Setenv("CI_POLL_TOKEN", "ci-poll-token-value")
	cfg := repoConfig()
	cfg.Repos[0].Token.Env = "CI_POLL_TOKEN"
	resolver, grants, err := buildCredentials(cfg, nil, "", "", nil, nil)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, grants, reg)
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	deterministic, err := buildCIPollExecutor(cfg, injector, ciPollTestRecorder{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildCIPollExecutor: %v", err)
	}
	return deterministic
}

// TestBuildCIPollExecutorSetsGiteaRepo proves ci-poll dispatches to Gitea when
// the gaggle's repo is Gitea, instead of defaulting to GitHub — the bug that
// made ci-poll hit api.github.com with a Gitea token and fail 401.
func TestBuildCIPollExecutorSetsGiteaRepo(t *testing.T) {
	t.Setenv("CI_POLL_TOKEN", "ci-poll-token-value")
	cfg := repoConfig()
	cfg.Repos[0].Token.Env = "CI_POLL_TOKEN"
	resolver, grants, err := buildCredentials(cfg, nil, "", "", nil, nil)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, grants, &escTestRegistrar{})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	giteaRepo := &instance.RepoRef{Provider: "gitea", BaseURL: "https://gitea.example.com", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "CI_POLL_TOKEN"}}
	exec, err := buildCIPollExecutor(cfg, injector, ciPollTestRecorder{}, nil, giteaRepo, nil)
	if err != nil {
		t.Fatalf("buildCIPollExecutor: %v", err)
	}
	e, ok := exec.(*ciPollKindExecutor)
	if !ok {
		t.Fatalf("executor type = %T, want *ciPollKindExecutor", exec)
	}
	if e.giteaRepo == nil || e.giteaRepo.BaseURL != "https://gitea.example.com" {
		t.Fatalf("giteaRepo not wired into ci-poll executor: %+v", e.giteaRepo)
	}
	if e.adoRepo != nil {
		t.Fatalf("adoRepo must be nil for a gitea ci-poll executor")
	}
}

func ciPollTestEnvelope(capabilities []string) apiv1.InvocationEnvelope {
	return apiv1.InvocationEnvelope{
		RepoRef:      apiv1.RepoRef{Owner: "acme", Name: "web"},
		Capabilities: capabilities,
		Inputs: map[string]interface{}{
			executor.InputKind:     executor.KindCIPoll,
			executor.InputPRNumber: "401",
		},
	}
}

func TestCIPollCredentialRequiresDeclaredCapability(t *testing.T) {
	deterministic := newCIPollWiringTestExecutor(t, &escTestRegistrar{})
	called := false
	prev := newPRPoller
	newPRPoller = func(string) executor.PRPoller {
		called = true
		return &ciPollFakePoller{}
	}
	t.Cleanup(func() { newPRPoller = prev })

	_, err := deterministic.Run(context.Background(), ciPollTestEnvelope(nil), apiv1.DeterministicRun{})
	if !errors.Is(err, credentials.ErrUndeclaredCapability) {
		t.Fatalf("Run error = %v, want ErrUndeclaredCapability", err)
	}
	if called {
		t.Fatal("PR poller constructed before capability admission")
	}
}

func TestCIPollCredentialAdmitsDeclaredCapability(t *testing.T) {
	reg := &escTestRegistrar{}
	deterministic := newCIPollWiringTestExecutor(t, reg)
	poller := &ciPollFakePoller{}
	var gotToken string
	prev := newPRPoller
	newPRPoller = func(token string) executor.PRPoller {
		gotToken = token
		return poller
	}
	t.Cleanup(func() { newPRPoller = prev })

	result, err := deterministic.Run(context.Background(), ciPollTestEnvelope([]string{string(capability.GitHubPRWrite)}), apiv1.DeterministicRun{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotToken != "ci-poll-token-value" {
		t.Fatalf("poller token = %q, want declared capability token", gotToken)
	}
	if !poller.called {
		t.Fatal("PR poller was not called")
	}
	if result.Outputs[executor.OutputCIStatus] != string(providers.CheckStatePassing) {
		t.Fatalf("outputs = %+v, want ciStatus=%q", result.Outputs, providers.CheckStatePassing)
	}
	if len(reg.registered) != 1 || string(reg.registered[0]) != "ci-poll-token-value" {
		t.Fatalf("registered secrets = %q, want the ci-poll token", reg.registered)
	}
}

type escFakeCommenter struct {
	gotReq providers.UpdateWorkItemRequest
}

func (f *escFakeCommenter) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	f.gotReq = req
	return providers.WorkItem{}, nil
}

// TestBuildEscalationNotifier is #312: the notifier is wired at the composition
// root for a repo-backed instance (so runner.Config.Escalation is no longer
// always nil), and nil for a repo-less instance (nothing to comment on).
func TestBuildEscalationNotifier(t *testing.T) {
	t.Run("nil for a repo-less instance", func(t *testing.T) {
		if n := buildEscalationNotifier(instance.Layout{}, &instance.Config{}, nil, nil); n != nil {
			t.Fatalf("expected a nil notifier for no repos, got %+v", n)
		}
	})
	t.Run("wired for a repo-backed instance", func(t *testing.T) {
		cfg := &instance.Config{Repos: []instance.RepoRef{
			{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "ESC_TOK"}},
		}}
		resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "ESC_TOK"}})
		if err != nil {
			t.Fatalf("NewResolver: %v", err)
		}
		n := buildEscalationNotifier(instance.Layout{}, cfg, resolver, &escTestRegistrar{})
		if n == nil {
			t.Fatal("expected a non-nil notifier for a repo-backed instance")
		}
		if n.Poster == nil {
			t.Fatal("expected a non-nil escalation poster")
		}
	})
}

// TestEscalationCommenterResolvesTokenPerCall is #312's rotation-safety +
// scrubbing property plus #544's multi-repo regression: the commenter resolves
// the request repository's token on each call (not captured at startup),
// registers it for scrubbing, and posts through a freshly-authenticated
// provider.
func TestEscalationCommenterResolvesTokenPerCall(t *testing.T) {
	t.Setenv("ESC_PRIMARY_TOK", "primary-token-value")
	t.Setenv("ESC_SECONDARY_TOK", "secondary-token-value")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{
		{Name: "acme/web", Env: "ESC_PRIMARY_TOK"},
		{Name: "acme/api", Env: "ESC_SECONDARY_TOK"},
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	reg := &escTestRegistrar{}

	fake := &escFakeCommenter{}
	var gotToken string
	prev := newEscalationPoster
	newEscalationPoster = func(token string) gate.Commenter { gotToken = token; return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	c := &escalationCommenter{resolver: resolver, reg: reg}
	repository := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}
	if _, err := c.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
		Repository: repository,
		ID:         "281",
		Comment:    "escalated",
	}); err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
	if gotToken != "secondary-token-value" {
		t.Fatalf("poster built with token %q, want the secondary repository token", gotToken)
	}
	if fake.gotReq.Repository != repository || fake.gotReq.ID != "281" || fake.gotReq.Comment != "escalated" {
		t.Fatalf("posted request = %+v", fake.gotReq)
	}
	var registered bool
	for _, s := range reg.registered {
		if string(s) == "secondary-token-value" {
			registered = true
		}
	}
	if !registered {
		t.Fatalf("resolved token not registered for scrubbing; registered=%v", reg.registered)
	}
}

func TestWithNeedsHumanAssignee(t *testing.T) {
	for _, provider := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
		t.Run(string(provider), func(t *testing.T) {
			repository := providers.RepositoryRef{Provider: provider}
			configured := withNeedsHumanAssignee(providers.UpdateWorkItemRequest{
				Repository: repository,
				AddLabels:  []string{providers.LabelNeedsHuman},
			}, "mason")
			if configured.Assignee == nil || *configured.Assignee != "mason" {
				t.Fatalf("configured Assignee = %v, want mason", configured.Assignee)
			}

			unconfigured := withNeedsHumanAssignee(providers.UpdateWorkItemRequest{
				Repository: repository,
				AddLabels:  []string{providers.LabelNeedsHuman},
			}, "")
			if unconfigured.Assignee != nil {
				t.Fatalf("unconfigured Assignee = %q, want nil", *unconfigured.Assignee)
			}
			if !slices.Equal(unconfigured.AddLabels, []string{providers.LabelNeedsHuman}) {
				t.Fatalf("unconfigured AddLabels = %v, want needs-human unchanged", unconfigured.AddLabels)
			}
		})
	}
}

// TestEscalationCommenterRoutesADOAwayFromGitHubToken verifies the ADO branch
// added for the Example.Repo loop: an ADO driving repo must NOT hit the
// GitHub token-resolve path (there is no static token for azure-cli auth, which
// previously produced ErrTokenRefNotFound and silently no-op'd the park/notify).
// With no instance config on disk, the ADO branch fails building its provider —
// the point is only that the GitHub poster is never consulted.
func TestEscalationCommenterRoutesADOAwayFromGitHubToken(t *testing.T) {
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter {
		t.Fatal("GitHub escalation poster must not be built for an ADO repo")
		return nil
	}
	t.Cleanup(func() { newEscalationPoster = prev })

	c := &escalationCommenter{resolver: resolver, reg: &escTestRegistrar{}, layout: instance.NewLayout(t.TempDir())}
	_, err = c.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
		Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "example-org", Project: "Example Service", Name: "Example.Repo"},
		ID:         "3169478",
		AddLabels:  []string{providers.LabelNeedsHuman},
	})
	if err == nil || !strings.Contains(err.Error(), "build ADO escalation provider") {
		t.Fatalf("UpdateWorkItem error = %v, want a build-ADO-provider error (ADO branch)", err)
	}
}

// TestADOParkRemovalLabelsTranslatesClaimMarker locks the claim-marker fix: an
// ADO park must remove the status-label form goobers/status:claimed (what
// ClaimWorkItem writes), not the GitHub plain LabelClaimed, while leaving other
// removals (LabelReady) untouched.
func TestADOParkRemovalLabelsTranslatesClaimMarker(t *testing.T) {
	got := adoParkRemovalLabels([]string{providers.LabelReady, providers.LabelClaimed})
	want := []string{providers.LabelReady, providers.StatusLabelFor(providers.WorkItemStatusClaimed)}
	if !slices.Equal(got, want) {
		t.Fatalf("adoParkRemovalLabels = %v, want %v", got, want)
	}
	if want[1] != "goobers/status:claimed" {
		t.Fatalf("ADO claim marker = %q, want goobers/status:claimed", want[1])
	}
}

// --- #353: open-PR-count refresher wiring ---

func cappedWorkflows() []apiv1.Workflow {
	return []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Readiness: apiv1.ReadinessConditions{MaxOpenPRs: 1}}}}
}

func repoConfig() *instance.Config {
	return &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "OPENPR_TOK"}},
	}}
}

// TestBuildOpenPRRefresher is #353: the refresher is built only when a repo is
// configured AND some workflow opts into the MaxOpenPRs cap — so an instance
// that doesn't use the cap grows no GitHub poller.
func TestBuildOpenPRRefresher(t *testing.T) {
	t.Run("nil for a repo-less instance", func(t *testing.T) {
		r, err := buildOpenPRRefresher(&instance.Config{}, cappedWorkflows(), &escTestRegistrar{}, nil, "", nil)
		if err != nil || r != nil {
			t.Fatalf("want nil,nil; got %v,%v", r, err)
		}
	})
	t.Run("nil when no workflow opts into the cap", func(t *testing.T) {
		wfs := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}}}}
		r, err := buildOpenPRRefresher(repoConfig(), wfs, &escTestRegistrar{}, nil, "", nil)
		if err != nil || r != nil {
			t.Fatalf("want nil,nil; got %v,%v", r, err)
		}
	})
	t.Run("built when a repo and a capped workflow are present", func(t *testing.T) {
		r, err := buildOpenPRRefresher(repoConfig(), cappedWorkflows(), &escTestRegistrar{}, nil, "", nil)
		if err != nil {
			t.Fatalf("buildOpenPRRefresher: %v", err)
		}
		if r == nil {
			t.Fatal("expected a non-nil refresher for a repo-backed, capped instance")
		}
	})
}

type fakeHeadLister struct{ heads []string }

func (f *fakeHeadLister) ListOpenPullRequests(context.Context, providers.RepositoryRef) ([]providers.OpenPRSummary, error) {
	prs := make([]providers.OpenPRSummary, 0, len(f.heads))
	for _, h := range f.heads {
		prs = append(prs, providers.OpenPRSummary{Head: h})
	}
	return prs, nil
}

// TestResolvingOpenPRListerResolvesTokenPerCall is #353's rotation-safety +
// scrubbing property: the lister resolves the org-repo token on each poll (not
// captured at startup), registers it for scrubbing, and lists through a freshly
// authenticated provider.
func TestResolvingOpenPRListerResolvesTokenPerCall(t *testing.T) {
	t.Setenv("OPENPR_TOK", "list-token-value")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "OPENPR_TOK"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	reg := &escTestRegistrar{}

	fake := &fakeHeadLister{heads: []string{"goobers/implementation/run-1"}}
	var gotToken string
	prev := newOpenPRProvider
	newOpenPRProvider = func(token string, _ ...func(*providers.GitHubProvider)) localscheduler.OpenPRLister {
		gotToken = token
		return fake
	}
	t.Cleanup(func() { newOpenPRProvider = prev })

	l := &resolvingOpenPRLister{ref: "acme/web", resolver: resolver, reg: reg}
	prs, err := l.ListOpenPullRequests(context.Background(), providers.RepositoryRef{Owner: "acme", Name: "web"})
	if err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}
	if gotToken != "list-token-value" {
		t.Fatalf("provider built with token %q, want the resolved token", gotToken)
	}
	if len(prs) != 1 || prs[0].Head != "goobers/implementation/run-1" {
		t.Fatalf("prs = %v", prs)
	}
	var registered bool
	for _, s := range reg.registered {
		if string(s) == "list-token-value" {
			registered = true
		}
	}
	if !registered {
		t.Fatalf("resolved token not registered for scrubbing; registered=%v", reg.registered)
	}
}

// blockedHandlerFakeCommenter records every UpdateWorkItem call (unlike
// escFakeCommenter, which only keeps the last) — buildBlockedHandler's
// multi-item fallback path needs every call visible.
type blockedHandlerFakeCommenter struct {
	calls []providers.UpdateWorkItemRequest
}

func (f *blockedHandlerFakeCommenter) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	f.calls = append(f.calls, req)
	return providers.WorkItem{}, nil
}

func blockedHandlerTestResolver(t *testing.T) credentials.Resolver {
	t.Helper()
	t.Setenv("BLOCKED_TOK", "blocked-token-value")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "BLOCKED_TOK"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return resolver
}

// TestBuildBlockedHandlerNilForRepoLessInstance mirrors
// TestBuildEscalationNotifier's repo-less case: no repo configured, no
// driving issue to comment on.
func TestBuildBlockedHandlerNilForRepoLessInstance(t *testing.T) {
	if h := buildBlockedHandler(instance.NewLayout(t.TempDir()), &instance.Config{}, nil, nil); h != nil {
		t.Fatalf("expected a nil handler for no repos, got %+v", h)
	}
}

// TestBuildBlockedHandlerKnownBlockersRecordsAndParks retains #552's learned
// dependency guard while applying #544's needs-human parking disposition.
func TestBuildBlockedHandlerKnownBlockersRecordsAndParks(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	var gotToken string
	prev := newEscalationPoster
	newEscalationPoster = func(token string) gate.Commenter {
		gotToken = token
		return fake
	}
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	t.Setenv("BLOCKED_SECONDARY_TOK", "blocked-secondary-token")
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
		{Provider: "github", Owner: "acme", Name: "api", Token: instance.TokenRef{Env: "BLOCKED_SECONDARY_TOK"}},
	}, NeedsHumanAssignee: "mason"}
	t.Setenv("BLOCKED_TOK", "blocked-primary-token")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{
		{Name: "acme/web", Env: "BLOCKED_TOK"},
		{Name: "acme/api", Env: "BLOCKED_SECONDARY_TOK"},
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	h := buildBlockedHandler(l, cfg, resolver, &escTestRegistrar{})
	if h == nil {
		t.Fatal("expected a non-nil handler for a repo-backed instance")
	}

	err = h(context.Background(), runner.BlockedOutcome{
		RunID:   "run-1",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "api", Branch: "main"},
		Stage:   "implement", ItemID: "510",
		Reason: "DEPENDENCY_NOT_MET: unmet prerequisite", Blockers: []string{"441", "442"},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("parking calls = %d, want 1", len(fake.calls))
	}
	got := fake.calls[0]
	if got.ID != "510" {
		t.Fatalf("request ID = %q, want 510", got.ID)
	}
	wantRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}
	if got.Repository != wantRepo {
		t.Fatalf("request repository = %+v, want secondary repository %+v", got.Repository, wantRepo)
	}
	if gotToken != "blocked-secondary-token" {
		t.Fatalf("parking token = %q, want secondary repository token", gotToken)
	}
	if len(got.AddLabels) != 1 || got.AddLabels[0] != providers.LabelNeedsHuman {
		t.Fatalf("AddLabels = %v, want [%s]", got.AddLabels, providers.LabelNeedsHuman)
	}
	if got.Assignee == nil || *got.Assignee != "mason" {
		t.Fatalf("Assignee = %v, want mason", got.Assignee)
	}
	wantRemoved := []string{providers.LabelReady, providers.LabelClaimed}
	if !slices.Equal(got.RemoveLabels, wantRemoved) {
		t.Fatalf("RemoveLabels = %v, want %v", got.RemoveLabels, wantRemoved)
	}
	if got.Comment != "" {
		t.Fatalf("comment = %q, want empty (the shared escalation notifier owns the comment)", got.Comment)
	}

	recs, err := loadBlockedRecords(blockedRecordsPath(l))
	if err != nil {
		t.Fatalf("loadBlockedRecords: %v", err)
	}
	rec, ok := recs[blockedRecordKey(wantRepo, "510")]
	if !ok {
		t.Fatal("expected a blocked.json record for item 510")
	}
	if rec.Repository != wantRepo || rec.ItemID != "510" || len(rec.Blockers) != 2 || rec.RunID != "run-1" {
		t.Fatalf("record = %+v, want blockers [441 442] from run-1", rec)
	}
}

func TestBuildBlockedHandlerRecordFailureStillParks(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.WriteFile(l.SchedulerDir(), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write scheduler blocker: %v", err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	h := buildBlockedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})

	err := h(context.Background(), runner.BlockedOutcome{
		RunID: "run-1", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage: "implement", ItemID: "510",
		Reason: "DEPENDENCY_NOT_MET: unmet prerequisite", Blockers: []string{"441"},
	})
	if err == nil || !strings.Contains(err.Error(), "record block for 510") {
		t.Fatalf("handler error = %v, want blocked-record failure", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("parking calls = %d, want 1 despite blocked-record failure", len(fake.calls))
	}
	got := fake.calls[0]
	wantRemoved := []string{providers.LabelReady, providers.LabelClaimed}
	if len(got.AddLabels) != 1 || got.AddLabels[0] != providers.LabelNeedsHuman ||
		!slices.Equal(got.RemoveLabels, wantRemoved) {
		t.Fatalf("parking request = %+v, want needs-human added and ready/claimed removed", got)
	}
}

func TestBuildBlockedHandlerEscalatesCircularDependency(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	if err := saveBlockedRecords(blockedRecordsPath(l), map[string]blockedRecord{
		blockedRecordKey(repo, "441"): {Repository: repo, ItemID: "441", Blockers: []string{"510"}, RunID: "prior-1"},
		blockedRecordKey(repo, "442"): {Repository: repo, ItemID: "442", Blockers: []string{"510"}, RunID: "prior-2"},
	}); err != nil {
		t.Fatalf("seed blocked records: %v", err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	h := buildBlockedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})

	err := h(context.Background(), runner.BlockedOutcome{
		RunID:   "run-cycle",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage:   "implement", ItemID: "510",
		Reason: "DEPENDENCY_NOT_MET: unmet prerequisite", Blockers: []string{"441", "442"},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	wantIDs := []string{"510", "441", "442"}
	if len(fake.calls) != len(wantIDs) {
		t.Fatalf("update calls = %d, want %d: %+v", len(fake.calls), len(wantIDs), fake.calls)
	}
	for i, got := range fake.calls {
		if got.ID != wantIDs[i] {
			t.Errorf("call %d ID = %q, want %q", i, got.ID, wantIDs[i])
		}
		if !slices.Equal(got.AddLabels, []string{providers.LabelNeedsHuman}) {
			t.Errorf("call %d AddLabels = %v, want [%s]", i, got.AddLabels, providers.LabelNeedsHuman)
		}
		wantRemoved := []string{providers.LabelReady, providers.LabelClaimed}
		if !slices.Equal(got.RemoveLabels, wantRemoved) {
			t.Errorf("call %d RemoveLabels = %v, want %v", i, got.RemoveLabels, wantRemoved)
		}
		for _, path := range []string{"#510 -> #441 -> #510", "#510 -> #442 -> #510"} {
			if !strings.Contains(got.Comment, path) {
				t.Errorf("call %d comment = %q, want ordered cycle %q", i, got.Comment, path)
			}
		}
		for _, itemID := range wantIDs {
			if !strings.Contains(got.Comment, "#"+itemID) {
				t.Errorf("call %d comment = %q, want affected issue #%s", i, got.Comment, itemID)
			}
		}
	}

	recs, err := loadBlockedRecords(blockedRecordsPath(l))
	if err != nil {
		t.Fatalf("loadBlockedRecords: %v", err)
	}
	if got := recs[blockedRecordKey(repo, "510")].Blockers; !slices.Equal(got, []string{"441", "442"}) {
		t.Fatalf("recorded blockers = %v, want [441 442]", got)
	}
}

func TestBuildBlockedHandlerScopesCyclesByRepository(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	var tokens []string
	prev := newEscalationPoster
	newEscalationPoster = func(token string) gate.Commenter {
		tokens = append(tokens, token)
		return fake
	}
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	webRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	apiRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}
	if err := saveBlockedRecords(blockedRecordsPath(l), map[string]blockedRecord{
		blockedRecordKey(webRepo, "441"): {
			Repository: webRepo, ItemID: "441", Blockers: []string{"999"}, RunID: "web-prior",
		},
		blockedRecordKey(apiRepo, "441"): {
			Repository: apiRepo, ItemID: "441", Blockers: []string{"510"}, RunID: "api-prior",
		},
	}); err != nil {
		t.Fatalf("seed blocked records: %v", err)
	}
	t.Setenv("BLOCKED_TOK", "web-token")
	t.Setenv("BLOCKED_API_TOK", "api-token")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{
		{Name: "acme/web", Env: "BLOCKED_TOK"},
		{Name: "acme/api", Env: "BLOCKED_API_TOK"},
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
		{Provider: "github", Owner: "acme", Name: "api", Token: instance.TokenRef{Env: "BLOCKED_API_TOK"}},
	}}
	handler := buildBlockedHandler(l, cfg, resolver, &escTestRegistrar{})

	if err := handler(context.Background(), runner.BlockedOutcome{
		RunID: "web-current", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage: "implement", ItemID: "510", Blockers: []string{"441"},
	}); err != nil {
		t.Fatalf("web handler: %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0].Repository != webRepo || fake.calls[0].ID != "510" ||
		fake.calls[0].Comment != "" ||
		!slices.Equal(fake.calls[0].AddLabels, []string{providers.LabelNeedsHuman}) ||
		!slices.Equal(fake.calls[0].RemoveLabels, []string{providers.LabelReady, providers.LabelClaimed}) {
		t.Fatalf("web calls = %+v, want one non-cycle parking update for web#510", fake.calls)
	}

	if err := handler(context.Background(), runner.BlockedOutcome{
		RunID: "api-current", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "api"},
		Stage: "implement", ItemID: "510", Blockers: []string{"441"},
	}); err != nil {
		t.Fatalf("api handler: %v", err)
	}
	if len(fake.calls) != 3 {
		t.Fatalf("calls = %+v, want web blocked comment plus two API cycle updates", fake.calls)
	}
	for i, wantID := range []string{"510", "441"} {
		got := fake.calls[i+1]
		if got.Repository != apiRepo || got.ID != wantID || got.Comment == "" {
			t.Errorf("API cycle call %d = %+v, want api#%s with a cycle comment", i, got, wantID)
		}
	}
	if want := []string{"web-token", "api-token", "api-token"}; !slices.Equal(tokens, want) {
		t.Fatalf("poster tokens = %v, want %v", tokens, want)
	}

	recs, err := loadBlockedRecords(blockedRecordsPath(l))
	if err != nil {
		t.Fatalf("loadBlockedRecords: %v", err)
	}
	for _, key := range []string{
		blockedRecordKey(webRepo, "441"),
		blockedRecordKey(webRepo, "510"),
		blockedRecordKey(apiRepo, "441"),
		blockedRecordKey(apiRepo, "510"),
	} {
		if _, ok := recs[key]; !ok {
			t.Errorf("blocked records missing %q: %+v", key, recs)
		}
	}
}

func TestBlockedCycleCommentIsBounded(t *testing.T) {
	paths := make([][]string, 20)
	for i := range paths {
		for j := 0; j < 100; j++ {
			paths[i] = append(paths[i], strings.Repeat("9", 100))
		}
	}
	comment := blockedCycleComment(paths, true)
	if len(comment) > maxBlockedCycleCommentLength {
		t.Fatalf("comment length = %d, want at most %d", len(comment), maxBlockedCycleCommentLength)
	}
	if !strings.Contains(comment, "additional cycle paths omitted") {
		t.Fatalf("comment = %q, want omitted-path notice", comment)
	}
	if !strings.Contains(comment, "cycle members omitted") {
		t.Fatalf("comment = %q, want explicit omitted-member notice", comment)
	}

	singleCycleComment := blockedCycleComment(paths[:1], false)
	if len(singleCycleComment) > maxBlockedCycleCommentLength {
		t.Fatalf("single-cycle comment length = %d, want at most %d", len(singleCycleComment), maxBlockedCycleCommentLength)
	}
	if !strings.Contains(singleCycleComment, "cycle members omitted") {
		t.Fatalf("single-cycle comment = %q, want explicit omitted-member notice", singleCycleComment)
	}
	if strings.Contains(singleCycleComment, "additional cycle paths omitted") {
		t.Fatalf("single-cycle comment = %q, did not want omitted-path notice", singleCycleComment)
	}
}

func TestBlockedCycleCommentPreservesLongSingleCycle(t *testing.T) {
	path := []string{
		"1001", "1002", "1003", "1004", "1005", "1006", "1007",
		"1008", "1009", "1010", "1011", "1012", "1013", "1014",
		"1015", "1016", "1017", "1018", "1019", "1020",
	}
	path = append(path, path[0])

	wantMembers := make([]string, len(path))
	for i, number := range path {
		wantMembers[i] = "#" + number
	}
	wantPath := strings.Join(wantMembers, cyclePathSeparator)
	comment := blockedCycleComment([][]string{path}, false)
	if !strings.Contains(comment, wantPath) {
		t.Fatalf("comment = %q, want complete ordered cycle %q", comment, wantPath)
	}
	if strings.Contains(comment, "cycle members omitted") {
		t.Fatalf("comment = %q, did not want member truncation", comment)
	}
}

func TestBlockedCycleCommentsNameEveryAffectedIssue(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	dependencies := make(map[string][]string)
	const nodes = 12
	for i := 0; i < nodes; i++ {
		itemID := fmt.Sprintf("%d", 500+i)
		for j := 0; j < nodes; j++ {
			dependencies[itemID] = append(dependencies[itemID], fmt.Sprintf("%d", 500+j))
		}
	}

	cycle := findBlockedCycle(blockedCycleTestRecords(repo, dependencies), blockedRecordKey(repo, "500"))
	if len(cycle.Paths) != maxBlockedCyclePaths || !cycle.MorePaths {
		t.Fatalf("cycle paths = %v, more = %v; want capped dense report", cycle.Paths, cycle.MorePaths)
	}
	comments := blockedCycleComments(cycle)
	if len(comments) != 1 {
		t.Fatalf("comments = %d, want dense 12-member cycle to fit in one report", len(comments))
	}
	for _, item := range cycle.Affected {
		if !strings.Contains(comments[0], "#"+item.ItemID) {
			t.Errorf("comment = %q, want affected issue #%s", comments[0], item.ItemID)
		}
	}
}

func TestBlockedCycleCommentsSplitCompleteMemberList(t *testing.T) {
	cycle := blockedCycleResult{
		Paths:     [][]string{{"10000", "10001", "10000"}},
		MorePaths: true,
	}
	for i := 0; i < 500; i++ {
		cycle.Affected = append(cycle.Affected, blockedCycleNode{ItemID: fmt.Sprintf("%d", 10000+i)})
	}

	comments := blockedCycleComments(cycle)
	if len(comments) < 3 {
		t.Fatalf("comments = %d, want primary report plus member follow-ups", len(comments))
	}
	combined := strings.Join(comments, "\n")
	for _, comment := range comments {
		if len(comment) > maxBlockedCycleCommentLength {
			t.Errorf("comment length = %d, want at most %d", len(comment), maxBlockedCycleCommentLength)
		}
	}
	for _, item := range cycle.Affected {
		if !strings.Contains(combined, "#"+item.ItemID) {
			t.Errorf("comments omitted affected issue #%s", item.ItemID)
		}
	}
}

func TestBuildBlockedHandlerEscalatesCircularDependencyForPRClaim(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	if err := saveBlockedRecords(blockedRecordsPath(l), map[string]blockedRecord{
		blockedRecordKey(repo, "956"): {Repository: repo, ItemID: "956", Blockers: []string{"955"}, RunID: "prior"},
	}); err != nil {
		t.Fatalf("seed blocked records: %v", err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	h := buildBlockedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})

	err := h(context.Background(), runner.BlockedOutcome{
		RunID:   "run-cycle",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage:   "implement", ItemID: "pr/955",
		Reason: "DEPENDENCY_NOT_MET: unmet prerequisite", Blockers: []string{"956"},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	wantIDs := []string{"955", "956"}
	if len(fake.calls) != len(wantIDs) {
		t.Fatalf("update calls = %d, want %d: %+v", len(fake.calls), len(wantIDs), fake.calls)
	}
	for i, got := range fake.calls {
		if got.ID != wantIDs[i] {
			t.Errorf("call %d ID = %q, want %q", i, got.ID, wantIDs[i])
		}
		if !strings.Contains(got.Comment, "#955 -> #956 -> #955") {
			t.Errorf("call %d comment = %q, want normalized ordered cycle", i, got.Comment)
		}
	}

	recs, err := loadBlockedRecords(blockedRecordsPath(l))
	if err != nil {
		t.Fatalf("loadBlockedRecords: %v", err)
	}
	if _, ok := recs[blockedRecordKey(repo, "pr/955")]; !ok {
		t.Fatal("blocked record did not retain PR claim key pr/955")
	}
}

// TestBuildBlockedHandlerNoBlockersParksNeedsHuman covers the unattributed
// path: no blocked.json record, but the same #539 parking disposition.
func TestBuildBlockedHandlerNoBlockersParksNeedsHuman(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	h := buildBlockedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})

	err := h(context.Background(), runner.BlockedOutcome{
		RunID: "run-1", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage: "implement", ItemID: "520",
		Reason: "waiting on an external dependency",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("parking calls = %d, want 1", len(fake.calls))
	}
	got := fake.calls[0]
	if got.ID != "520" {
		t.Fatalf("request ID = %q, want 520", got.ID)
	}
	if len(got.AddLabels) != 1 || got.AddLabels[0] != providers.LabelNeedsHuman {
		t.Fatalf("AddLabels = %v, want [%s]", got.AddLabels, providers.LabelNeedsHuman)
	}
	wantRemoved := []string{providers.LabelReady, providers.LabelClaimed}
	if !slices.Equal(got.RemoveLabels, wantRemoved) {
		t.Fatalf("RemoveLabels = %v, want %v", got.RemoveLabels, wantRemoved)
	}
	if got.Comment != "" {
		t.Fatalf("comment = %q, want empty (the shared escalation notifier owns the comment)", got.Comment)
	}

	recs, err := loadBlockedRecords(blockedRecordsPath(l))
	if err != nil {
		t.Fatalf("loadBlockedRecords: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("blocked.json = %+v, want empty — nothing for #552 to skip/self-heal on an unattributed block", recs)
	}
}

// TestBuildBlockedHandlerResolvesItemFromClaimLedgerWhenEmpty proves a run
// started without StartInput.Item (scheduled/fan-out implementation runs
// claim their item mid-run) still notifies the right issue: the handler
// falls back to the claim ledger by RunID, since the run's claims are still
// held at the point the handler runs (before FinalizeTerminal releases them).
func TestBuildBlockedHandlerResolvesItemFromClaimLedgerWhenEmpty(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	if ok, _, err := ledger.Claim("530", "run-fanout", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	h := buildBlockedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})

	err = h(context.Background(), runner.BlockedOutcome{
		RunID: "run-fanout", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage: "implement", Reason: "blocked", Blockers: []string{"441"},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0].ID != "530" {
		t.Fatalf("calls = %+v, want exactly one call for item 530 (resolved via the claim ledger)", fake.calls)
	}
}

// TestPRClaimBlockedFlowNormalizesProviderID proves the claim ledger and
// blocked-record store retain the namespaced PR key while provider operations
// use GitHub's bare issue/PR number.
func TestPRClaimBlockedFlowNormalizesProviderID(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	if ok, _, err := ledger.Claim("pr/955", "run-remediate", "pr-remediation", time.Hour); err != nil || !ok {
		t.Fatalf("seed PR claim: ok=%v err=%v", ok, err)
	}

	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	resolver := blockedHandlerTestResolver(t)
	reg := &escTestRegistrar{}
	h := buildBlockedHandler(l, cfg, resolver, reg)
	outcome := runner.BlockedOutcome{
		RunID: "run-remediate", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage: "implement", Reason: "blocked on issue 956", Blockers: []string{"956"},
	}
	if err := h(context.Background(), outcome); err != nil {
		t.Fatalf("blocked handler: %v", err)
	}

	ids, err := claimedItemIDsForRun(l, "run-remediate")
	if err != nil {
		t.Fatalf("claimedItemIDsForRun: %v", err)
	}
	if !slices.Equal(ids, []string{"pr/955"}) {
		t.Fatalf("claimed item IDs = %v, want [pr/955]", ids)
	}
	notifier := buildEscalationNotifier(instance.Layout{}, cfg, resolver, reg)
	repository := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	if err := notifier.NotifyStageEscalated(context.Background(), repository, ids[0], outcome.Stage, outcome.Reason); err != nil {
		t.Fatalf("NotifyStageEscalated: %v", err)
	}

	if len(fake.calls) != 2 {
		t.Fatalf("provider calls = %+v, want parking and notification", fake.calls)
	}
	if fake.calls[0].ID != "955" || fake.calls[1].ID != "955" {
		t.Fatalf("provider IDs = [%q %q], want bare PR number 955", fake.calls[0].ID, fake.calls[1].ID)
	}
	if fake.calls[1].Comment == "" {
		t.Fatal("notification comment is empty")
	}
	recs, err := loadBlockedRecords(blockedRecordsPath(l))
	if err != nil {
		t.Fatalf("loadBlockedRecords: %v", err)
	}
	if _, ok := recs[blockedRecordKey(repository, "pr/955")]; !ok {
		t.Fatalf("blocked records = %+v, want repository-scoped PR claim key", recs)
	}
	if _, ok := recs[blockedRecordKey(repository, "955")]; ok {
		t.Fatalf("blocked records = %+v, bare provider ID must not replace the claim key", recs)
	}
}

// TestBuildBlockedHandlerNoClaimIsANoop proves a producer/schedule-triggered
// run (no Item, no claim to resolve) is a clean no-op — the journaled
// blocked_by_agent cause and escalated phase are the whole story; nothing to
// notify.
func TestBuildBlockedHandlerNoClaimIsANoop(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	h := buildBlockedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})

	err := h(context.Background(), runner.BlockedOutcome{
		RunID: "run-producer", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage: "curate", Reason: "blocked",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %+v, want none (no driving item anywhere)", fake.calls)
	}
}

// TestBuildFailedHandlerNilForRepoLessInstance mirrors buildBlockedHandler's
// repo-less case: no repo configured, no driving item to trace on (#1054).
func TestBuildFailedHandlerNilForRepoLessInstance(t *testing.T) {
	if h := buildFailedHandler(instance.NewLayout(t.TempDir()), &instance.Config{}, nil, nil); h != nil {
		t.Fatalf("expected a nil handler for no repos, got %+v", h)
	}
}

// TestBuildFailedHandlerPostsTraceCommentWithoutNeedsHuman is #1054's core: a
// run that ends terminal `failed` while claiming an item leaves a human-visible
// trace — exactly one comment on the driving item carrying the terminal cause
// and the run id — and, crucially, applies NO labels (goobers:needs-human stays
// reserved for the escalated/park path). The item is resolved from the claim
// ledger by run id, the same fallback buildBlockedHandler uses, since the
// implementation/pr-remediation runs that hit this claim their item mid-run.
func TestBuildFailedHandlerPostsTraceCommentWithoutNeedsHuman(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	if ok, _, err := ledger.Claim("463", "run-timeout", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	h := buildFailedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})
	if h == nil {
		t.Fatal("expected a non-nil handler for a repo-backed instance")
	}

	err = h(context.Background(), runner.FailedOutcome{
		RunID:   "run-timeout",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Stage:   "implement",
		Cause:   "runner: execute stage \"implement\": harness: copilot-cli: session timed out after 30m0s (attempt 2/2)",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("comment calls = %d, want 1", len(fake.calls))
	}
	got := fake.calls[0]
	if got.ID != "463" {
		t.Fatalf("request ID = %q, want 463 (resolved via the claim ledger)", got.ID)
	}
	wantRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	if got.Repository != wantRepo {
		t.Fatalf("request repository = %+v, want %+v", got.Repository, wantRepo)
	}
	if len(got.AddLabels) != 0 || len(got.RemoveLabels) != 0 {
		t.Fatalf("labels = add %v / remove %v, want NONE — a `failed` terminal must not touch labels (needs-human stays reserved for escalation)", got.AddLabels, got.RemoveLabels)
	}
	if !strings.Contains(got.Comment, "run-timeout") {
		t.Fatalf("comment = %q, want it to carry the run id", got.Comment)
	}
	if !strings.Contains(got.Comment, "session timed out after 30m0s") {
		t.Fatalf("comment = %q, want it to carry the terminal failure cause", got.Comment)
	}
	if strings.Contains(got.Comment, providers.LabelNeedsHuman) && (len(got.AddLabels) > 0) {
		t.Fatalf("comment = %q, must not apply needs-human", got.Comment)
	}
}

// TestBuildFailedHandlerNormalizesPRClaimID proves the pr/<n> claim key (used by
// pr-remediation, one of the two workflows that hit the #1054 timeout) is
// normalized to its bare provider number when the trace comment is posted —
// mirroring the blocked flow, via escalationCommenter.UpdateWorkItem.
func TestBuildFailedHandlerNormalizesPRClaimID(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	if ok, _, err := ledger.Claim("pr/955", "run-remediate-fail", "pr-remediation", time.Hour); err != nil || !ok {
		t.Fatalf("seed PR claim: ok=%v err=%v", ok, err)
	}

	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	h := buildFailedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})

	err = h(context.Background(), runner.FailedOutcome{
		RunID:   "run-remediate-fail",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage:   "implement",
		Cause:   "session timed out after 30m0s",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0].ID != "955" {
		t.Fatalf("calls = %+v, want exactly one comment on bare PR number 955", fake.calls)
	}
}

// TestBuildFailedHandlerNoClaimIsANoop proves a producer/schedule-triggered run
// (no claim to resolve) is a clean no-op — the journaled run_failed cause and
// the failed phase are the whole story; nothing to trace (#1054).
func TestBuildFailedHandlerNoClaimIsANoop(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	h := buildFailedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})

	err := h(context.Background(), runner.FailedOutcome{
		RunID:   "run-producer",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage:   "query-backlog",
		Cause:   "some walk-level failure",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %+v, want none (no driving item anywhere)", fake.calls)
	}
}

// TestBranchNamespacesByGaggle covers #1010: two gaggles with different
// configured branch namespaces (and one that omits it) each resolve to the
// correct run-branch namespace root, normalized to a trailing slash, so a
// multi-gaggle instance no longer assumes a single "goobers/" across all
// gaggles.
func TestBranchNamespacesByGaggle(t *testing.T) {
	set := &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{
			{ObjectMeta: metav1.ObjectMeta{Name: "default-gaggle"}, Spec: apiv1.GaggleSpec{}},
			{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: apiv1.GaggleSpec{BranchNamespace: "acme/"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "no-slash"}, Spec: apiv1.GaggleSpec{BranchNamespace: "widgets"}},
		},
	}
	got := branchNamespacesByGaggle(set)
	want := map[string]string{
		"default-gaggle": "goobers/", // omitted → providers.DefaultBranchNamespace
		"acme":           "acme/",
		"no-slash":       "widgets/", // normalized to a trailing slash
	}
	if len(got) != len(want) {
		t.Fatalf("branchNamespacesByGaggle = %+v, want %+v", got, want)
	}
	for gaggle, wantNS := range want {
		if got[gaggle] != wantNS {
			t.Errorf("gaggle %q namespace = %q, want %q", gaggle, got[gaggle], wantNS)
		}
	}
}

func TestSelfIdentitiesByGaggle(t *testing.T) {
	cfg := &instance.Config{SelfIdentity: "instance-bot"}
	set := &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{
			{ObjectMeta: metav1.ObjectMeta{Name: "inherits"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "overrides"}, Spec: apiv1.GaggleSpec{SelfIdentity: "gaggle-bot"}},
		},
	}

	got := selfIdentitiesByGaggle(cfg, set)
	want := map[string]string{
		"inherits":  "instance-bot",
		"overrides": "gaggle-bot",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("selfIdentitiesByGaggle = %#v, want %#v", got, want)
	}
}

func TestRequireLabelsByGaggle(t *testing.T) {
	set := &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{
			{ObjectMeta: metav1.ObjectMeta{Name: "no-default"}, Spec: apiv1.GaggleSpec{}},
			{ObjectMeta: metav1.ObjectMeta{Name: "frontend"}, Spec: apiv1.GaggleSpec{RequireLabels: []string{"goobers:ready", "area:frontend"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "billing"}, Spec: apiv1.GaggleSpec{RequireLabels: []string{"area:billing"}}},
		},
	}
	got := requireLabelsByGaggle(set)
	want := map[string]string{
		"no-default": "",
		"frontend":   "goobers:ready,area:frontend",
		"billing":    "area:billing",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("requireLabelsByGaggle = %#v, want %#v", got, want)
	}
}
