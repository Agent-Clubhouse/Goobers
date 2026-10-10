package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// The operator-message conformance suite runs identical request vectors against
// the local-runner and Temporal-engine daemon fixtures and compares the
// normalized journal-visible request, mode, acknowledgement, and terminal
// outcome semantics. A vector may declare an intentional adapter divergence
// (documented in docs/reference/engine-parity.md); such a vector must actually
// diverge and is pinned by per-adapter assertions, so neither an undeclared
// divergence nor a stale declaration can pass silently.

const (
	omConformanceRunID = "conformance-run"
	omConformanceStage = "implement"
)

type omConformanceAdapter struct {
	name   string
	engine bool
}

var omConformanceAdapters = []omConformanceAdapter{
	{name: "local"},
	{name: "temporal", engine: true},
}

type omConformanceTarget struct {
	modes []string
	err   error
}

type omConformanceSeed struct {
	key          string
	agent        string
	mode         string
	acknowledged bool
}

type omConformanceVector struct {
	name string
	// agents are live in one run/stage/attempt visit before submission.
	agents  []string
	targets map[string]omConformanceTarget
	seed    *omConformanceSeed
	// terminal finishes the run before any submission.
	terminal bool
	// restartStage starts a later visit of the stage so the seeded agent
	// addresses become stale.
	restartStage bool
	submissions  []func(address func(agent string) string) httpapi.OperatorMessageSubmissionRequest
	// divergence names an intentional, documented adapter capability
	// difference; empty means the adapters must be equivalent.
	divergence string
	check      func(t *testing.T, adapter omConformanceAdapter, obs omConformanceObservation)
}

type omConformanceResponse struct {
	Accepted bool
	Status   int
	State    apiv1.OperatorMessageState
}

type omConformanceRecord struct {
	RequestID            string
	IdempotencyKey       string
	TargetAgent          string
	PrincipalRef         string
	Purpose              string
	ContentText          string
	DeliveryMode         string
	Expires              bool
	State                apiv1.OperatorMessageState
	AckPrincipalRef      string
	OutcomeStatus        apiv1.OperatorMessageOutcomeStatus
	OutcomeCode          string
	OutcomeDetail        string
	OutcomeEmbedsRequest bool
}

type omConformanceObservation struct {
	Responses  []omConformanceResponse
	Records    []omConformanceRecord
	Deliveries map[string]int
	persisted  []byte
}

type omConformanceFixture struct {
	layoutRunDir string
	service      *daemonRunJournalService
	addresses    map[string]string
	targets      map[string]*fakeOperatorMessageTarget
	receipts     *operatorReceiptSink
}

func omConformanceSubmit(agent string, mutate func(*httpapi.OperatorMessageSubmissionRequest)) func(func(string) string) httpapi.OperatorMessageSubmissionRequest {
	return func(address func(string) string) httpapi.OperatorMessageSubmissionRequest {
		request := operatorMessageRequest(omConformanceRunID, "key-"+agent,
			httpapi.Principal{Subject: "operator", Issuer: "issuer", Roles: []httpapi.Role{httpapi.RoleOperate}})
		request.TargetAddress = address(agent)
		if mutate != nil {
			mutate(&request)
		}
		return request
	}
}

func omConformanceVectors() []omConformanceVector {
	secret := "ghp_" + strings.Repeat("c", 36)
	betweenAndInterrupt := []string{invoke.OperatorMessageModeInterruptAndContinue, invoke.OperatorMessageModeBetweenTurn}
	return []omConformanceVector{
		{
			name:   "selected agent isolation and between-turn negotiation",
			agents: []string{"agent-a", "agent-b"},
			targets: map[string]omConformanceTarget{
				"agent-a": {modes: betweenAndInterrupt},
				"agent-b": {modes: betweenAndInterrupt},
			},
			submissions: []func(func(string) string) httpapi.OperatorMessageSubmissionRequest{omConformanceSubmit("agent-a", nil)},
			check: func(t *testing.T, _ omConformanceAdapter, obs omConformanceObservation) {
				omConformanceWantDelivered(t, obs, invoke.OperatorMessageModeBetweenTurn)
				if obs.Deliveries["agent-a"] != 1 || obs.Deliveries["agent-b"] != 0 {
					t.Fatalf("deliveries = %v, want only agent-a", obs.Deliveries)
				}
			},
		},
		{
			name:        "interrupt-and-continue negotiation",
			agents:      []string{"agent-a"},
			targets:     map[string]omConformanceTarget{"agent-a": {modes: []string{invoke.OperatorMessageModeInterruptAndContinue}}},
			submissions: []func(func(string) string) httpapi.OperatorMessageSubmissionRequest{omConformanceSubmit("agent-a", nil)},
			check: func(t *testing.T, _ omConformanceAdapter, obs omConformanceObservation) {
				omConformanceWantDelivered(t, obs, invoke.OperatorMessageModeInterruptAndContinue)
			},
		},
		{
			name:    "duplicate idempotency key resolves to the original record",
			agents:  []string{"agent-a"},
			targets: map[string]omConformanceTarget{"agent-a": {modes: []string{invoke.OperatorMessageModeBetweenTurn}}},
			submissions: []func(func(string) string) httpapi.OperatorMessageSubmissionRequest{
				omConformanceSubmit("agent-a", nil),
				omConformanceSubmit("agent-a", func(r *httpapi.OperatorMessageSubmissionRequest) { r.Content.Text = "changed" }),
			},
			check: func(t *testing.T, _ omConformanceAdapter, obs omConformanceObservation) {
				omConformanceWantDelivered(t, obs, invoke.OperatorMessageModeBetweenTurn)
				if len(obs.Responses) != 2 || !obs.Responses[0].Accepted || obs.Responses[1].Accepted ||
					obs.Deliveries["agent-a"] != 1 || obs.Records[0].ContentText != "please review" {
					t.Fatalf("duplicate observation = %+v", obs)
				}
			},
		},
		{
			name:    "expired request is terminal without delivery",
			agents:  []string{"agent-a"},
			targets: map[string]omConformanceTarget{"agent-a": {modes: []string{invoke.OperatorMessageModeBetweenTurn}}},
			submissions: []func(func(string) string) httpapi.OperatorMessageSubmissionRequest{
				omConformanceSubmit("agent-a", func(r *httpapi.OperatorMessageSubmissionRequest) {
					expired := time.Now().Add(-time.Minute)
					r.ExpiresAt = &expired
				}),
			},
			check: func(t *testing.T, _ omConformanceAdapter, obs omConformanceObservation) {
				omConformanceWantOutcome(t, obs, apiv1.OperatorMessageExpired, "request_expired")
				if obs.Responses[0].Accepted || !obs.Records[0].OutcomeEmbedsRequest || !obs.Records[0].Expires ||
					obs.Deliveries["agent-a"] != 0 {
					t.Fatalf("expired observation = %+v", obs)
				}
			},
		},
		{
			name:        "retry resumes an accepted undelivered request once",
			agents:      []string{"agent-a"},
			targets:     map[string]omConformanceTarget{"agent-a": {modes: []string{invoke.OperatorMessageModeBetweenTurn}}},
			seed:        &omConformanceSeed{key: "key-agent-a", agent: "agent-a", mode: invoke.OperatorMessageModeBetweenTurn},
			submissions: []func(func(string) string) httpapi.OperatorMessageSubmissionRequest{omConformanceSubmit("agent-a", nil), omConformanceSubmit("agent-a", nil)},
			check: func(t *testing.T, _ omConformanceAdapter, obs omConformanceObservation) {
				omConformanceWantDelivered(t, obs, invoke.OperatorMessageModeBetweenTurn)
				if obs.Responses[0].Accepted || obs.Responses[1].Accepted || obs.Deliveries["agent-a"] != 1 {
					t.Fatalf("resumed observation = %+v", obs)
				}
			},
		},
		{
			name:        "retry completes an acknowledged request without redelivery",
			agents:      []string{"agent-a"},
			targets:     map[string]omConformanceTarget{"agent-a": {modes: []string{invoke.OperatorMessageModeBetweenTurn}}},
			seed:        &omConformanceSeed{key: "key-agent-a", agent: "agent-a", mode: invoke.OperatorMessageModeBetweenTurn, acknowledged: true},
			submissions: []func(func(string) string) httpapi.OperatorMessageSubmissionRequest{omConformanceSubmit("agent-a", nil)},
			check: func(t *testing.T, _ omConformanceAdapter, obs omConformanceObservation) {
				omConformanceWantDelivered(t, obs, invoke.OperatorMessageModeBetweenTurn)
				if obs.Responses[0].Accepted || obs.Deliveries["agent-a"] != 0 {
					t.Fatalf("acknowledged retry observation = %+v", obs)
				}
			},
		},
		{
			name:        "adapter delivery failure is typed and never redelivered",
			agents:      []string{"agent-a"},
			targets:     map[string]omConformanceTarget{"agent-a": {modes: []string{invoke.OperatorMessageModeBetweenTurn}, err: errors.New("adapter unavailable")}},
			submissions: []func(func(string) string) httpapi.OperatorMessageSubmissionRequest{omConformanceSubmit("agent-a", nil), omConformanceSubmit("agent-a", nil)},
			check: func(t *testing.T, _ omConformanceAdapter, obs omConformanceObservation) {
				omConformanceWantOutcome(t, obs, apiv1.OperatorMessageFailed, "delivery_failed")
				if obs.Records[0].AckPrincipalRef != "" || obs.Deliveries["agent-a"] != 1 || obs.Responses[1].Accepted {
					t.Fatalf("failure observation = %+v", obs)
				}
			},
		},
		{
			name:        "adapter cancellation is typed",
			agents:      []string{"agent-a"},
			targets:     map[string]omConformanceTarget{"agent-a": {modes: []string{invoke.OperatorMessageModeInterruptAndContinue}, err: context.Canceled}},
			submissions: []func(func(string) string) httpapi.OperatorMessageSubmissionRequest{omConformanceSubmit("agent-a", nil)},
			check: func(t *testing.T, _ omConformanceAdapter, obs omConformanceObservation) {
				omConformanceWantOutcome(t, obs, apiv1.OperatorMessageFailed, "delivery_canceled")
			},
		},
		{
			name:    "unauthorized principal is rejected with preserved authority",
			agents:  []string{"agent-a"},
			targets: map[string]omConformanceTarget{"agent-a": {modes: []string{invoke.OperatorMessageModeBetweenTurn}}},
			submissions: []func(func(string) string) httpapi.OperatorMessageSubmissionRequest{
				omConformanceSubmit("agent-a", func(r *httpapi.OperatorMessageSubmissionRequest) {
					r.Principal.Roles = []httpapi.Role{httpapi.RoleView}
				}),
			},
			check: func(t *testing.T, _ omConformanceAdapter, obs omConformanceObservation) {
				omConformanceWantOutcome(t, obs, apiv1.OperatorMessageRejected, "not_authorized")
				if obs.Responses[0].Status != http.StatusForbidden || obs.Records[0].PrincipalRef != "issuer:operator" ||
					obs.Deliveries["agent-a"] != 0 {
					t.Fatalf("unauthorized observation = %+v", obs)
				}
			},
		},
		{
			name:   "foreign run address is refused before journaling",
			agents: []string{"agent-a"},
			submissions: []func(func(string) string) httpapi.OperatorMessageSubmissionRequest{
				omConformanceSubmit("agent-a", func(r *httpapi.OperatorMessageSubmissionRequest) {
					r.TargetAddress = strings.Replace(r.TargetAddress, omConformanceRunID, "other-run", 1)
				}),
			},
			check: func(t *testing.T, _ omConformanceAdapter, obs omConformanceObservation) {
				if len(obs.Responses) != 1 || obs.Responses[0].Status != http.StatusBadRequest || len(obs.Records) != 0 {
					t.Fatalf("foreign address observation = %+v", obs)
				}
			},
		},
		{
			name:    "secret-bearing request is persisted scrubbed",
			agents:  []string{"agent-a"},
			targets: map[string]omConformanceTarget{"agent-a": {modes: []string{invoke.OperatorMessageModeBetweenTurn}}},
			submissions: []func(func(string) string) httpapi.OperatorMessageSubmissionRequest{
				omConformanceSubmit("agent-a", func(r *httpapi.OperatorMessageSubmissionRequest) {
					r.IdempotencyKey = "key-" + secret
					r.Content.Text = "Authorization: Bearer " + secret
				}),
			},
			check: func(t *testing.T, _ omConformanceAdapter, obs omConformanceObservation) {
				omConformanceWantDelivered(t, obs, invoke.OperatorMessageModeBetweenTurn)
				records, err := json.Marshal(obs.Records)
				if err != nil {
					t.Fatal(err)
				}
				assertOperatorMessageRepresentationScrubbed(t, "normalized records", records, secret)
				assertOperatorMessageRepresentationScrubbed(t, "persisted run journal", obs.persisted, secret)
				if !strings.HasPrefix(obs.Records[0].IdempotencyKey, "scrubbed:sha256:") {
					t.Fatalf("idempotency key was not canonicalized: %q", obs.Records[0].IdempotencyKey)
				}
			},
		},
		{
			name:        "live target without a reachable adapter",
			agents:      []string{"agent-a"},
			submissions: []func(func(string) string) httpapi.OperatorMessageSubmissionRequest{omConformanceSubmit("agent-a", nil)},
			divergence:  "local records next-attempt without an outcome; Temporal rejects live_delivery_unsupported",
			check: func(t *testing.T, adapter omConformanceAdapter, obs omConformanceObservation) {
				if adapter.engine {
					omConformanceWantOutcome(t, obs, apiv1.OperatorMessageRejected, "live_delivery_unsupported")
					return
				}
				omConformanceWantPendingNextAttempt(t, obs)
			},
		},
		{
			name:        "unknown target address",
			agents:      []string{"agent-a"},
			submissions: []func(func(string) string) httpapi.OperatorMessageSubmissionRequest{omConformanceSubmit("agent-gone", nil)},
			divergence:  "local records next-attempt without an outcome; Temporal rejects target_unavailable",
			check: func(t *testing.T, adapter omConformanceAdapter, obs omConformanceObservation) {
				if adapter.engine {
					omConformanceWantOutcome(t, obs, apiv1.OperatorMessageRejected, "target_unavailable")
					return
				}
				omConformanceWantPendingNextAttempt(t, obs)
			},
		},
		{
			name:         "stale target address after the stage restarted",
			agents:       []string{"agent-a"},
			restartStage: true,
			submissions:  []func(func(string) string) httpapi.OperatorMessageSubmissionRequest{omConformanceSubmit("agent-a", nil)},
			divergence:   "local records next-attempt without an outcome; Temporal rejects target_unavailable",
			check: func(t *testing.T, adapter omConformanceAdapter, obs omConformanceObservation) {
				if adapter.engine {
					omConformanceWantOutcome(t, obs, apiv1.OperatorMessageRejected, "target_unavailable")
					return
				}
				omConformanceWantPendingNextAttempt(t, obs)
			},
		},
		{
			name:        "terminated run",
			agents:      []string{"agent-a"},
			terminal:    true,
			submissions: []func(func(string) string) httpapi.OperatorMessageSubmissionRequest{omConformanceSubmit("agent-a", nil)},
			divergence:  "local records next-attempt without an outcome; Temporal rejects target_terminal",
			check: func(t *testing.T, adapter omConformanceAdapter, obs omConformanceObservation) {
				if adapter.engine {
					omConformanceWantOutcome(t, obs, apiv1.OperatorMessageRejected, "target_terminal")
					return
				}
				omConformanceWantPendingNextAttempt(t, obs)
			},
		},
	}
}

func TestOperatorMessageLocalTemporalConformance(t *testing.T) {
	for _, vector := range omConformanceVectors() {
		t.Run(vector.name, func(t *testing.T) {
			observations := make(map[string]omConformanceObservation, len(omConformanceAdapters))
			for _, adapter := range omConformanceAdapters {
				t.Run(adapter.name, func(t *testing.T) {
					obs := runOMConformanceVector(t, adapter, vector)
					vector.check(t, adapter, obs)
					observations[adapter.name] = obs
				})
			}
			if t.Failed() {
				return
			}
			local, temporal := observations["local"], observations["temporal"]
			equivalent := reflect.DeepEqual(local.Responses, temporal.Responses) &&
				reflect.DeepEqual(local.Records, temporal.Records) &&
				reflect.DeepEqual(local.Deliveries, temporal.Deliveries)
			switch {
			case vector.divergence == "" && !equivalent:
				t.Fatalf("undeclared adapter divergence:\nlocal    = %+v %+v %v\ntemporal = %+v %+v %v",
					local.Responses, local.Records, local.Deliveries, temporal.Responses, temporal.Records, temporal.Deliveries)
			case vector.divergence != "" && equivalent:
				t.Fatalf("declared divergence %q no longer diverges; remove the declaration and its documentation", vector.divergence)
			}
		})
	}
}

func runOMConformanceVector(t *testing.T, adapter omConformanceAdapter, vector omConformanceVector) omConformanceObservation {
	t.Helper()
	fixture := newOMConformanceFixture(t, adapter, vector)
	address := func(agent string) string {
		if value, ok := fixture.addresses[agent]; ok {
			return value
		}
		return operatorMessageAgentAddress(t, omConformanceRunID, omConformanceStage, 1, agent, fixture.startedSeq(t))
	}
	obs := omConformanceObservation{Deliveries: make(map[string]int)}
	for _, submission := range vector.submissions {
		response, err := fixture.service.SubmitOperatorMessage(context.Background(), submission(address))
		obs.Responses = append(obs.Responses, omConformanceResponse{
			Accepted: response.Accepted, Status: omConformanceStatus(t, err), State: response.Record.State,
		})
	}
	for agent, target := range fixture.targets {
		obs.Deliveries[agent] = len(target.deliveries)
		for _, delivery := range target.deliveries {
			if delivery.TargetAddress != fixture.addresses[agent] {
				t.Fatalf("agent %s received delivery addressed to %q", agent, delivery.TargetAddress)
			}
		}
	}
	reader, err := journal.OpenRead(fixture.layoutRunDir)
	if err != nil {
		t.Fatal(err)
	}
	if vector.restartStage {
		resolution, err := reader.ResolveAgentAddress(fixture.addresses["agent-a"])
		if err != nil || resolution.Status != journal.AgentAddressStale {
			t.Fatalf("restarted-stage address resolution = %+v, %v; want stale", resolution, err)
		}
	}
	records, err := reader.OperatorMessages()
	if err != nil {
		t.Fatal(err)
	}
	reverse := make(map[string]string, len(fixture.addresses))
	for agent, value := range fixture.addresses {
		reverse[value] = agent
	}
	for _, record := range records {
		obs.Records = append(obs.Records, normalizeOMConformanceRecord(record, reverse))
	}
	obs.persisted = readOMConformanceRunDir(t, fixture.layoutRunDir)
	if adapter.engine {
		assertOMConformanceReceipts(t, fixture.receipts, records, obs.Responses, vector.terminal)
	}
	return obs
}

func newOMConformanceFixture(t *testing.T, adapter omConformanceAdapter, vector omConformanceVector) *omConformanceFixture {
	t.Helper()
	layout := crossRunTestLayout(t)
	identity := journal.RunIdentity{
		RunID: omConformanceRunID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: crossRunTestGaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}
	if adapter.engine {
		identity.Driver = journal.DriverEngine
	}
	run, err := journal.Create(layout.ForGaggle(crossRunTestGaggle).RunsDir(), identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if run != nil {
			_ = run.Close()
		}
	}()
	fixture := &omConformanceFixture{
		layoutRunDir: filepath.Join(layout.ForGaggle(crossRunTestGaggle).RunsDir(), omConformanceRunID),
		addresses:    make(map[string]string),
		targets:      make(map[string]*fakeOperatorMessageTarget),
	}
	seedOMConformanceJournal(t, run, fixture, vector)
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	run = nil
	for agent, spec := range vector.targets {
		fixture.targets[agent] = registerOperatorMessageTarget(t, fixture.addresses[agent], spec.modes, spec.err)
	}
	fixture.service = newDaemonRunJournalService(layout, nil)
	if adapter.engine {
		writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) {
			return layout.ForGaggle(gaggle).RunsDir(), gaggle == crossRunTestGaggle
		}, livejournal.WithScrubber(journal.NewPatternScrubber()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(writer.Close)
		fixture.receipts = &operatorReceiptSink{}
		fixture.service.operatorMessages.Writer = writer
		fixture.service.operatorMessages.Deliverer = fixture.receipts
	}
	return fixture
}

func seedOMConformanceJournal(t *testing.T, run *journal.Run, fixture *omConformanceFixture, vector omConformanceVector) {
	t.Helper()
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: omConformanceStage, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	startedSeq := run.Seq()
	now := time.Now().UTC()
	for _, agent := range vector.agents {
		if err := run.Append(journal.Event{
			Type: journal.EventAgentLifecycle, Stage: omConformanceStage, Attempt: 1,
			Agent: &journal.AgentProvenance{
				Schema: "goobers.dev/journal/agent/v1", ID: agent, RunID: omConformanceRunID, Stage: omConformanceStage, Attempt: 1,
				Lifecycle: journal.AgentWaiting, StartedAt: now, UpdatedAt: now, Fidelity: journal.AgentFidelityFull,
			},
		}); err != nil {
			t.Fatal(err)
		}
		fixture.addresses[agent] = operatorMessageAgentAddress(t, omConformanceRunID, omConformanceStage, 1, agent, startedSeq)
	}
	if seed := vector.seed; seed != nil {
		request := acceptedOperatorMessageRequest(seed.key, fixture.addresses[seed.agent], seed.mode)
		request.PrincipalRef = "issuer:operator"
		if _, accepted, err := run.AcceptOperatorMessage(request); err != nil || !accepted {
			t.Fatalf("seed accept = %v, %v", accepted, err)
		}
		if seed.acknowledged {
			if _, err := run.AcknowledgeOperatorMessage(operatorMessageAcknowledgement(request)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if vector.restartStage {
		if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: omConformanceStage, Attempt: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if vector.terminal {
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *omConformanceFixture) startedSeq(t *testing.T) uint64 {
	t.Helper()
	for _, value := range f.addresses {
		address, err := journal.ParseAgentAddress(value)
		if err != nil {
			t.Fatal(err)
		}
		seq, err := address.StageStartedSeq()
		if err != nil {
			t.Fatal(err)
		}
		return seq
	}
	t.Fatal("conformance vector has no live agents")
	return 0
}

func omConformanceStatus(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var planeErr *httpapi.InterventionError
	if errors.As(err, &planeErr) {
		return planeErr.Status
	}
	t.Fatalf("unexpected submission error: %v", err)
	return 0
}

func normalizeOMConformanceRecord(record apiv1.OperatorMessageRecord, agents map[string]string) omConformanceRecord {
	target := record.Request.TargetAddress
	if agent, ok := agents[target]; ok {
		target = agent
	}
	normalized := omConformanceRecord{
		RequestID:      record.Request.RequestID,
		IdempotencyKey: record.Request.IdempotencyKey,
		TargetAgent:    target,
		PrincipalRef:   record.Request.PrincipalRef,
		Purpose:        record.Request.Purpose,
		ContentText:    record.Request.Content.Text,
		DeliveryMode:   record.Request.DeliveryMode,
		Expires:        record.Request.ExpiresAt != nil,
		State:          record.State,
	}
	if ack := record.Acknowledgement; ack != nil {
		normalized.AckPrincipalRef = ack.PrincipalRef
	}
	if outcome := record.Outcome; outcome != nil {
		normalized.OutcomeStatus = outcome.Status
		normalized.OutcomeCode = outcome.Code
		normalized.OutcomeDetail = outcome.Detail
		normalized.OutcomeEmbedsRequest = outcome.Request != nil
	}
	return normalized
}

func readOMConformanceRunDir(t *testing.T, dir string) []byte {
	t.Helper()
	var persisted []byte
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		persisted = append(persisted, data...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return persisted
}

// assertOMConformanceReceipts pins the Temporal-only receipt surface: every
// open-run submission that succeeded commits exactly one receipt carrying the
// replayed terminal disposition by content-free reference, and terminal runs
// never attempt a workflow update.
func assertOMConformanceReceipts(t *testing.T, sink *operatorReceiptSink, records []apiv1.OperatorMessageRecord, responses []omConformanceResponse, terminal bool) {
	t.Helper()
	if terminal {
		if len(sink.receipts) != 0 {
			t.Fatalf("terminal run produced workflow receipts: %+v", sink.receipts)
		}
		return
	}
	succeeded := 0
	for _, response := range responses {
		if response.Status == 0 {
			succeeded++
		}
	}
	if len(sink.receipts) != succeeded {
		t.Fatalf("workflow receipts = %d, want one per successful open-run submission (%d)", len(sink.receipts), succeeded)
	}
	byReference := make(map[string]apiv1.OperatorMessageRecord, len(records))
	for _, record := range records {
		byReference[engine.OperatorMessageReference(omConformanceRunID, record.Request.IdempotencyKey)] = record
	}
	for _, receipt := range sink.receipts {
		record, ok := byReference[receipt.Reference]
		if !ok || record.Outcome == nil || receipt.State != record.State || receipt.Code != record.Outcome.Code {
			t.Fatalf("receipt %+v does not match journal record %+v", receipt, record)
		}
	}
}

func omConformanceWantOutcome(t *testing.T, obs omConformanceObservation, status apiv1.OperatorMessageOutcomeStatus, code string) {
	t.Helper()
	if len(obs.Records) != 1 {
		t.Fatalf("records = %+v, want one", obs.Records)
	}
	record := obs.Records[0]
	if record.State != apiv1.OperatorMessageState(status) || record.OutcomeStatus != status || record.OutcomeCode != code {
		t.Fatalf("record = %+v, want %s/%s", record, status, code)
	}
	for _, response := range obs.Responses {
		if response.Status == 0 && response.State != record.State {
			t.Fatalf("response state %q differs from journal state %q", response.State, record.State)
		}
	}
}

func omConformanceWantDelivered(t *testing.T, obs omConformanceObservation, mode string) {
	t.Helper()
	omConformanceWantOutcome(t, obs, apiv1.OperatorMessageDelivered, "")
	record := obs.Records[0]
	if record.DeliveryMode != mode || record.AckPrincipalRef != record.PrincipalRef ||
		record.PrincipalRef != "issuer:operator" || record.TargetAgent != "agent-a" {
		t.Fatalf("delivered record = %+v, want mode %s acknowledged by the submitting principal", record, mode)
	}
}

func omConformanceWantPendingNextAttempt(t *testing.T, obs omConformanceObservation) {
	t.Helper()
	if len(obs.Records) != 1 || len(obs.Responses) != 1 || !obs.Responses[0].Accepted {
		t.Fatalf("observation = %+v, want one accepted record", obs)
	}
	record := obs.Records[0]
	if record.State != apiv1.OperatorMessageAccepted || record.DeliveryMode != invoke.OperatorMessageModeNextAttempt ||
		record.OutcomeStatus != "" || record.AckPrincipalRef != "" {
		t.Fatalf("record = %+v, want pending next-attempt", record)
	}
}
