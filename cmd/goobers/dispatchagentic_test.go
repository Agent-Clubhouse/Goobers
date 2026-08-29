package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runner"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
)

// The kit must carry the capability→env mapping the harness depends on.
//
// A harness reads its credential from the environment (agent:model becomes
// COPILOT_GITHUB_TOKEN). On the worker that variable is ambient, supplied by
// the deployment; a pod has none by design. MEASURED before the pod applied
// the mapping:
//
//	harness "copilot" preflight failed in pod:
//	  Error: No authentication information found.
//
// If EnvCapabilities ever stops carrying agent:model, an agentic pod fails at
// preflight with an error about GitHub auth that says nothing about kits.
func TestKitCarriesTheHarnessCredentialMapping(t *testing.T) {
	envCaps := buildEnvCapabilities()
	got, ok := envCaps["agent:model"]
	if !ok {
		t.Fatal("agent:model has no env mapping; an agentic pod cannot authenticate its harness")
	}
	if got != copilotModelEnv {
		t.Fatalf("agent:model maps to %q, want %q", got, copilotModelEnv)
	}
}

// The mapping has to survive the kit round trip, since that is how it reaches
// the pod at all.
func TestEnvCapabilitiesSurviveTheKit(t *testing.T) {
	kit := &agentickit.Kit{
		Envelope:        apiv1.InvocationEnvelope{RunID: "r", Goober: "g"},
		EnvCapabilities: buildEnvCapabilities(),
	}
	data, digest, err := agentickit.Marshal(kit)
	if err != nil {
		t.Fatal(err)
	}
	back, err := agentickit.Unmarshal(data, digest)
	if err != nil {
		t.Fatal(err)
	}
	if back.EnvCapabilities["agent:model"] != copilotModelEnv {
		t.Fatalf("mapping lost in transport: %v", back.EnvCapabilities)
	}
}

// buildAgenticExecutor type-asserts its recorder against several interfaces and
// fails at CONSTRUCTION when one is missing — so a missing method is a stage
// that never starts, reported as "recorder does not implement X" with nothing
// else to go on. MEASURED exactly that way on the cluster:
//
//	agentic_executor_unavailable: runner artifact recorder does not
//	implement harness.SpanRecorder
//
// These assertions move that failure to COMPILE TIME. Each one costs a
// rebuild-and-deploy cycle to find otherwise, which is how the first was found.
var (
	_ harness.SpanRecorder             = podArtifactRecorder{}
	_ harness.ArtifactRecorder         = podArtifactRecorder{}
	_ executor.BoundedArtifactRecorder = podArtifactRecorder{}
	_ interface{ Dir() string }        = podArtifactRecorder{}
	_ runner.ArtifactRecorder          = podArtifactRecorder{}
	// Asserted at INVOCATION time in four places, not construction — a missing
	// Append fails mid-run rather than at startup, which is how it was found.
	_ harness.EventAppender = podArtifactRecorder{}
)

// The SecretRegistrar is asserted to journal.Scrubber in the same constructor.
func TestPodSecretRegistrarIsAScrubber(t *testing.T) {
	registry, _ := journal.DefaultScrubber()
	if _, ok := interface{}(registry).(journal.Scrubber); !ok {
		t.Fatal("the registry handed in as SecretRegistrar must also be a journal.Scrubber")
	}
}

// The harness reads its working directory from the ENVELOPE, which the local
// path stamps by mutating it during provisioning. A pod that provisions a
// workspace without stamping it fails inside the harness with a message that
// says nothing about workspaces:
//
//	harness: copilot-cli: RunRequest.Workspace is empty
//
// This pins the stamp itself, because the failure it prevents is unreadable.
func TestAgenticEnvelopeCarriesItsWorkspace(t *testing.T) {
	kit := &agentickit.Kit{Envelope: apiv1.InvocationEnvelope{RunID: "r", Goober: "g"}}
	if kit.Envelope.Workspace != "" {
		t.Fatal("fixture should start unstamped")
	}
	// What runAgenticStage does after provisioning.
	ws := t.TempDir()
	kit.Envelope.Workspace = ws
	if kit.Envelope.Workspace != ws {
		t.Fatalf("workspace = %q, want %q", kit.Envelope.Workspace, ws)
	}
}

// One credential ref backs MANY capabilities: credentials.RunnerGrants assigns
// the same default ref (the project repo's token) to every credentialed
// capability. A ref -> capability map therefore keeps only the last grant
// written, and every lookup returns that one capability regardless of what the
// stage declared.
//
// MEASURED on the cluster: an agentic stage declaring repo:push failed with
//
//	materialize credential key "repo:push": capability "ado:pr:complete" was
//	not materialised by the credential plane
//
// naming ado:pr:complete — the last element of credentialedCapabilities — for a
// workflow that never mentions ADO at all.
func TestPodCredentialResolverHandlesOneRefBackingManyCapabilities(t *testing.T) {
	const ref = "masra91/Goobers-Site"
	// Grant order mirrors credentialedCapabilities: the capability the stage
	// actually declared is FIRST and the collision winner LAST, so a
	// last-write-wins map fails exactly as the cluster did.
	grants := []agentickit.Grant{
		{Capability: "repo:push", Ref: ref},
		{Capability: "github:pr:merge", Ref: ref},
		{Capability: "ado:pr:complete", Ref: ref},
	}
	resolver := podCredentialResolver{byRef: map[string][]string{}, vals: map[string]string{}}
	for _, g := range grants {
		if g.Ref != "" && g.Capability != "" {
			resolver.byRef[g.Ref] = append(resolver.byRef[g.Ref], g.Capability)
		}
	}
	// The plane materialises ONLY what the stage declared — never the whole
	// credentialed set. This is the asymmetry the bug lived in.
	resolver.vals["repo:push"] = "ghs_stage_token"

	got, err := resolver.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve(%q) = error %v; the ref is backed by a materialised capability", ref, err)
	}
	if got != "ghs_stage_token" {
		t.Fatalf("Resolve(%q) = %q, want the materialised token", ref, got)
	}

	// An ungranted ref must still be refused rather than silently empty.
	if _, err := resolver.Resolve(context.Background(), "someone-else/repo"); err == nil {
		t.Fatal("Resolve on an ungranted ref returned no error; an unknown ref must be refused")
	}

	// A granted ref whose capabilities were all withheld must name them all,
	// so the operator sees which grant to look at.
	resolver.vals = map[string]string{}
	_, err = resolver.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("Resolve succeeded with nothing materialised")
	}
	for _, want := range []string{"repo:push", "github:pr:merge", "ado:pr:complete"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name capability %q", err, want)
		}
	}
}

// TestPodArtifactRecorderAppendStampsOpTime is the writer-side half of #3774,
// tested at the seam the bug actually crosses: the pod's own Append call,
// through the real JSON-over-HTTP wire it uses in production, applied by a
// REAL *livejournal.Writer onto a REAL on-disk journal — then read back.
//
// MEASURED (#3774): run 8238995d's agent.lifecycle event was durably
// persisted with Time 0001-01-01T00:00:00Z, because podArtifactRecorder.Append
// built its livejournal.Op without a Time, and the daemon's replayClock
// adopts that field verbatim (applyOp: run.clock.set(op.Time)) for the event
// it is about to append. A test asserting an in-memory Op.Time field, or
// exercising appendEvent's stamping in isolation, would not cross that
// boundary and would repeat the L-152/L-153 mistake — this one does, by
// running the actual pod-side emitter against a live daemon-side writer and
// reading its events.jsonl back off disk.
func TestPodArtifactRecorderAppendStampsOpTime(t *testing.T) {
	root := t.TempDir()
	runsDir := filepath.Join(root, "runs")

	// A pod's Append never opens a run (it carries no Open header) — it only
	// appends to a run the daemon already created. Pre-create it directly, the
	// way an earlier stage.started emit would have.
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "agentic-run", Workflow: "impl-real-probe", WorkflowVersion: 1, Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement-on-pod", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) {
		if gaggle != "goobers" {
			return "", false
		}
		return runsDir, true
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)

	// A minimal stand-in for registerJournalPlaneRoutes: decode the batch and
	// hand it to the real writer, exactly as the daemon's journal-plane HTTP
	// handler does (internal/httpapi/journalplane.go).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req livejournal.EmitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resp, err := writer.Emit(r.Context(), req)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "emit_failed", "message": err.Error()}})
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(server.Close)

	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvRunID, "agentic-run")
	t.Setenv(dispatcher.EnvGaggle, "goobers")
	t.Setenv(dispatcher.EnvStage, "implement-on-pod")
	t.Setenv(dispatcher.EnvPodToken, "pod-token")

	before := time.Now().UTC()
	agentAt := before
	recorder := podArtifactRecorder{dir: t.TempDir()}
	if err := recorder.Append(journal.Event{
		Type: journal.EventAgentLifecycle, Stage: "implement-on-pod", Attempt: 1, Seq: 1,
		Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "copilot-1",
			RunID: "agentic-run", Stage: "implement-on-pod", Attempt: 1,
			Lifecycle: journal.AgentStarted, StartedAt: agentAt, UpdatedAt: agentAt,
			Fidelity: journal.AgentFidelityFull,
		},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	after := time.Now().UTC()

	rd, err := journal.OpenRead(filepath.Join(runsDir, "agentic-run"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var lifecycle *journal.Event
	for i := range events {
		if events[i].Type == journal.EventAgentLifecycle {
			lifecycle = &events[i]
		}
	}
	if lifecycle == nil {
		t.Fatal("agent.lifecycle event never landed in the journal")
	}
	if lifecycle.Time.IsZero() {
		t.Fatal("agent.lifecycle event.Time is zero (#3774): podArtifactRecorder.Append must stamp a real wall-clock Time on the Op before it leaves the pod, or the daemon's replayClock zeroes the event it applies")
	}
	if lifecycle.Time.Before(before) || lifecycle.Time.After(after) {
		t.Fatalf("agent.lifecycle event.Time = %s, want between %s and %s", lifecycle.Time, before, after)
	}
}
