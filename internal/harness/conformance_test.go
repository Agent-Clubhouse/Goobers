package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpconfig"
)

// This file is the conformance suite #2776 asks for: every capability
// dimension that's diverged silently between adapters in the past (tools
// allowlist #1471, goobers-io #2774, declared mcpServers #1492) gets one
// generic test, run identically against every entry in conformanceAdapters.
// A capability added for one adapter and not the other fails here instead of
// needing its own follow-up issue discovered weeks later.
//
// Known, already-accepted divergences are declared in knownGaps below, not
// silently absent from coverage — see TestKnownGapsAreDeclared.

// conformanceAdapter is one production-shaped adapter construction (mirrors
// cmd/goobers/runnerwiring.go's buildHarnessRegistry) plus the small set of
// per-adapter fixture values a dimension test needs. The mechanism each
// dimension test proves is adapter-agnostic; the concrete tool/env-var names
// a real CLI expects are not, so those live here as data, not per-adapter
// test-function duplication.
type conformanceAdapter struct {
	name string
	// build constructs a real adapter with runner installed and, when
	// selfBin != "", SelfBin set — pass "" to build the "harness never
	// wired this run" case the goobers-io negative control needs.
	build func(runner ProcessRunner, selfBin string) Adapter
	// stub seeds whatever ambient state this adapter's Run() unconditionally
	// depends on so a bare Run() doesn't fail on ambient-environment
	// specifics unrelated to the dimension under test (e.g. claude-code's
	// unconditional credential seeding, #2775).
	stub func(t *testing.T)

	// Tool allowlist dimension (a): declaring Tools: []string{"telemetry"}
	// must make toolIncluded reachable and toolExcluded unreachable.
	toolIncluded string
	toolExcluded string

	// Isolation dimension (e): the env var this adapter redirects to
	// isolate ambient user config from a run, and whether that redirection
	// is unconditional (every run) or conditional (only under some other
	// condition, e.g. Sandbox != nil) — see the copilot-cli knownGaps entry.
	isolationEnvVar        string
	isolationUnconditional bool
}

func conformanceAdapters() []conformanceAdapter {
	return []conformanceAdapter{
		{
			name: "copilot-cli",
			build: func(runner ProcessRunner, selfBin string) Adapter {
				return &CopilotAdapter{
					Command: []string{"copilot"},
					Runner:  runner,
					EnvCapabilities: map[string]string{
						"agent:model":   "TEST_MODEL_TOKEN",
						"contents:read": "TEST_CONTEXT_TOKEN",
					},
					SelfBin: selfBin,
				}
			},
			stub:                   func(t *testing.T) {},
			toolIncluded:           "view",
			toolExcluded:           "apply_patch",
			isolationEnvVar:        "COPILOT_HOME",
			isolationUnconditional: false,
		},
		{
			name: "claude-code",
			build: func(runner ProcessRunner, selfBin string) Adapter {
				return &ClaudeAdapter{
					Command: []string{"claude"},
					Runner:  runner,
					EnvCapabilities: map[string]string{
						"agent:model":   "TEST_MODEL_TOKEN",
						"contents:read": "TEST_CONTEXT_TOKEN",
					},
					SelfBin: selfBin,
				}
			},
			stub:                   func(t *testing.T) { stubClaudeCredentialsHome(t) },
			toolIncluded:           "Read",
			toolExcluded:           "Write",
			isolationEnvVar:        "CLAUDE_CONFIG_DIR",
			isolationUnconditional: true,
		},
	}
}

// TestConformanceCoveredAdaptersAreExhaustive pins conformanceAdapters
// itself against ConformanceCoveredAdapterNames — the exported list
// cmd/goobers's registry-completeness check compares against. If someone
// adds an adapter to one table and not the other, this fails immediately
// rather than silently under-covering.
func TestConformanceCoveredAdaptersAreExhaustive(t *testing.T) {
	var got []string
	for _, ca := range conformanceAdapters() {
		got = append(got, ca.name)
	}
	slices.Sort(got)
	want := append([]string(nil), ConformanceCoveredAdapterNames()...)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("conformanceAdapters() names = %v, ConformanceCoveredAdapterNames() = %v", got, want)
	}
}

func conformanceRunner(t *testing.T) *fakeProcessRunner {
	t.Helper()
	return &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
}

// TestConformanceToolAllowlist is dimension (a): a non-empty Spec.Tools
// declaration must constrain the session on every adapter, and a tool
// omitted from it must be unreachable — the negative control that would
// have caught claude-code silently ignoring req.Tools before #1471.
func TestConformanceToolAllowlist(t *testing.T) {
	for _, ca := range conformanceAdapters() {
		t.Run(ca.name, func(t *testing.T) {
			ca.stub(t)
			workspace := t.TempDir()
			runner := conformanceRunner(t)
			adapter := ca.build(runner, "")
			_, err := adapter.Run(context.Background(), RunRequest{
				Envelope:       testEnvelope(workspace),
				Workspace:      workspace,
				CompletionPath: DefaultResultPath,
				Tools:          []string{"telemetry"},
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			command := strings.Join(runner.lastReq.Command, " ")
			if !strings.Contains(command, ca.toolIncluded) {
				t.Errorf("telemetry-group tool %q is not reachable: %v", ca.toolIncluded, runner.lastReq.Command)
			}
			if strings.Contains(command, ca.toolExcluded) {
				t.Errorf("tool %q omitted from the telemetry group is reachable: %v", ca.toolExcluded, runner.lastReq.Command)
			}
		})
	}
}

// TestConformanceMCPServersMaterializeAndAreReachable is dimension (b),
// positive half: a declared server's credential must resolve and reach the
// subprocess environment under the shared GOOBERS_MCP_CREDENTIAL_<i>_<j>
// naming convention — identical across adapters — and never appear in argv.
func TestConformanceMCPServersMaterializeAndAreReachable(t *testing.T) {
	for _, ca := range conformanceAdapters() {
		t.Run(ca.name, func(t *testing.T) {
			ca.stub(t)
			workspace := t.TempDir()
			runner := conformanceRunner(t)
			adapter := ca.build(runner, "")
			creds := mcpTestCredentials(t, "agent:model", "model-token", "contents:read", "context-secret")
			_, err := adapter.Run(context.Background(), RunRequest{
				Envelope:       testEnvelope(workspace, "agent:model", "contents:read"),
				Workspace:      workspace,
				CompletionPath: DefaultResultPath,
				Credentials:    creds,
				MCPServers: []apiv1.MCPServer{{
					Name:    "context",
					Command: "context-server",
					CredentialRefs: []apiv1.MCPCredentialRef{
						{Capability: "contents:read", Env: "CONTEXT_TOKEN"},
					},
				}},
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !slices.Contains(runner.lastReq.Env, "GOOBERS_MCP_CREDENTIAL_0_0=context-secret") {
				t.Errorf("resolved MCP credential missing from subprocess environment: %v", runner.lastReq.Env)
			}
			for _, arg := range runner.lastReq.Command {
				if strings.Contains(arg, "context-secret") {
					t.Fatalf("credential leaked into argv: %v", runner.lastReq.Command)
				}
			}
		})
	}
}

// TestConformanceMCPServersRejectCredentialExposureToLocalSibling is
// dimension (b), negative-control half: a local stdio server not authorized
// for a sibling's credential must be rejected before materialization, on
// every adapter — live-verified during #1492 that claude-code's stdio
// servers share the full parent process environment exactly like Copilot's,
// so this isn't a defensive carry-over, it's an equally real requirement.
func TestConformanceMCPServersRejectCredentialExposureToLocalSibling(t *testing.T) {
	for _, ca := range conformanceAdapters() {
		t.Run(ca.name, func(t *testing.T) {
			ca.stub(t)
			workspace := t.TempDir()
			adapter := ca.build(conformanceRunner(t), "")
			creds := mcpTestCredentials(t, "agent:model", "model-token", mcpconfig.BYOCredentialKey("vendor-api"), "vendor-secret")
			_, err := adapter.Run(context.Background(), RunRequest{
				Envelope:       testEnvelope(workspace, "agent:model"),
				Workspace:      workspace,
				CompletionPath: DefaultResultPath,
				Credentials:    creds,
				MCPServers: []apiv1.MCPServer{
					{Name: "local-context", Command: "context-server"},
					{
						Name: "vendor-context",
						URL:  "https://vendor.example.test/mcp",
						CredentialRefs: []apiv1.MCPCredentialRef{
							{Kind: apiv1.MCPCredentialKindBYO, Ref: "vendor-api", Header: "Authorization"},
						},
					},
				},
			})
			if err == nil || !strings.Contains(err.Error(), `local stdio server "local-context" cannot isolate credential "mcp:vendor-api"`) {
				t.Fatalf("Run error = %v, want local credential-isolation rejection", err)
			}
		})
	}
}

// TestConformanceGoobersIORegistersOnlyWhenWired is dimension (c): an
// eligible, wired run must advertise goobers-io's tools in its prompt (the
// "## goobers-io tools" section is shared, adapter-agnostic code — see
// prompt.go), and an eligible-but-never-wired run (no SelfBin) must not —
// the negative control that's the actual #2774 bug fix: an adapter must
// never promise tools it never registered.
func TestConformanceGoobersIORegistersOnlyWhenWired(t *testing.T) {
	for _, ca := range conformanceAdapters() {
		t.Run(ca.name, func(t *testing.T) {
			ca.stub(t)
			for _, tc := range []struct {
				name           string
				selfBin        string
				wantAdvertised bool
			}{
				{name: "wired", selfBin: "/usr/local/bin/goobers", wantAdvertised: true},
				{name: "never wired", selfBin: "", wantAdvertised: false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					workspace := t.TempDir()
					runner := conformanceRunner(t)
					adapter := ca.build(runner, tc.selfBin)
					envelope := testEnvelope(workspace)
					envelope.Inputs = map[string]interface{}{InputArtifactFile: "findings.md"}
					_, err := adapter.Run(context.Background(), RunRequest{
						Envelope:       envelope,
						Workspace:      workspace,
						CompletionPath: DefaultResultPath,
					})
					if err != nil {
						t.Fatalf("Run: %v", err)
					}
					prompt, err := os.ReadFile(filepath.Join(workspace, ".goobers", "prompt.md"))
					if err != nil {
						t.Fatal(err)
					}
					advertised := strings.Contains(string(prompt), "## goobers-io tools") && strings.Contains(string(prompt), "publish_output")
					if advertised != tc.wantAdvertised {
						t.Errorf("advertised goobers-io tools = %v, want %v:\n%s", advertised, tc.wantAdvertised, prompt)
					}
				})
			}
		})
	}
}

// TestConformanceModelOptionsRejectedAtAdmission is dimension (d): an
// invalid harness option must be rejected through the shared
// harness.ValidateConfig entry point — before any process spawns — on every
// adapter, not passed through to the CLI to fail (or silently misbehave)
// mid-run.
func TestConformanceModelOptionsRejectedAtAdmission(t *testing.T) {
	for _, ca := range conformanceAdapters() {
		t.Run(ca.name, func(t *testing.T) {
			adapter := ca.build(conformanceRunner(t), "")
			options := testHarnessOptions(t, map[string]interface{}{"totally-bogus-option": "x"})
			if err := ValidateConfig(adapter, "", options); err == nil {
				t.Fatal("ValidateConfig accepted an unknown harness option")
			}
		})
	}
}

// TestConformanceAmbientIsolation is dimension (e): whether ambient user
// config reaches a run. Both adapters redirect a config-home env var away
// from whatever's ambient, but — a genuine, previously-undeclared asymmetry
// this suite surfaces — claude-code does so unconditionally on every run
// (#2775), while Copilot's redirection only fires when Sandbox != nil or MCP
// servers are declared (a bare, unsandboxed, MCP-free Copilot run still
// inherits the ambient COPILOT_HOME). isolationUnconditional records which
// behavior is real per adapter so this test documents current reality
// precisely instead of asserting a false uniform contract; see the
// knownGaps entry below for why the copilot-cli gap isn't fixed here.
func TestConformanceAmbientIsolation(t *testing.T) {
	for _, ca := range conformanceAdapters() {
		t.Run(ca.name, func(t *testing.T) {
			ca.stub(t)
			ambientValue := filepath.Join(t.TempDir(), "ambient")
			t.Setenv(ca.isolationEnvVar, ambientValue)
			workspace := t.TempDir()
			runner := conformanceRunner(t)
			adapter := ca.build(runner, "")
			_, err := adapter.Run(context.Background(), RunRequest{
				Envelope:       testEnvelope(workspace),
				Workspace:      workspace,
				CompletionPath: DefaultResultPath,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			redirected := false
			for _, entry := range runner.lastReq.Env {
				if name, value, ok := strings.Cut(entry, "="); ok && name == ca.isolationEnvVar {
					redirected = value != ambientValue
				}
			}
			if redirected != ca.isolationUnconditional {
				t.Errorf("%s redirected unconditionally = %v, want %v (see knownGaps)", ca.isolationEnvVar, redirected, ca.isolationUnconditional)
			}
		})
	}
}

// knownGap records an accepted, deliberate divergence between adapters for
// one capability dimension — declared and reasoned about explicitly here,
// not silently absent from this suite's coverage (#2776's core ask).
type knownGap struct {
	dimension     string
	adapter       string
	detail        string
	trackingIssue string
}

var knownGaps = []knownGap{
	{
		dimension:     "tool allowlist",
		adapter:       "claude-code",
		detail:        `no "github" tool-group equivalent — Copilot resolves it to github-mcp-server-* tool names, claude-code has none; a goober declaring tools:[github] on claude-code falls through expandToolGroup unexpanded`,
		trackingIssue: "#1471",
	},
	{
		dimension:     "MCP servers",
		adapter:       "claude-code",
		detail:        `no per-server MCP "tools" sub-allowlist field — once a declared server is registered, every tool it reports is reachable regardless of the goober's Spec.Tools; confirmed live that --tools/--allowedTools don't gate MCP-server tools on claude-code at all`,
		trackingIssue: "#2774, #1492",
	},
	{
		dimension:     "ambient isolation",
		adapter:       "copilot-cli",
		detail:        `COPILOT_HOME redirection only fires when Sandbox != nil or MCP servers are declared — a bare, unsandboxed, MCP-free Copilot run still inherits ambient COPILOT_HOME, unlike claude-code's unconditional CLAUDE_CONFIG_DIR redirect on every run (#2775). Surfaced by this suite (#2776), not fixed here — out of scope for "build a conformance suite"; tracked separately`,
		trackingIssue: "#2816",
	},
}

// TestKnownGapsAreDeclared makes every accepted adapter divergence visible in
// `go test -v` output — the acceptance criterion that a deliberate gap must
// be "expressible as a declaration... never inferred from a missing test".
func TestKnownGapsAreDeclared(t *testing.T) {
	if len(knownGaps) == 0 {
		t.Fatal("knownGaps is empty — either every dimension is fully conformant on every adapter, or a gap went undeclared")
	}
	for _, gap := range knownGaps {
		t.Logf("declared gap: [%s / %s] %s (tracked: %s)", gap.dimension, gap.adapter, gap.detail, gap.trackingIssue)
		if gap.dimension == "" || gap.adapter == "" || gap.detail == "" || gap.trackingIssue == "" {
			t.Errorf("knownGaps entry has an empty field: %#v", gap)
		}
		if !slices.Contains(ConformanceCoveredAdapterNames(), gap.adapter) {
			t.Errorf("knownGaps entry names adapter %q, which isn't conformance-covered: %#v", gap.adapter, gap)
		}
	}
}
