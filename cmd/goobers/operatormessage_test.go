package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/converter"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

type operatorMessageCommitSink struct {
	mu     sync.Mutex
	events []journal.CommittedEvent
}

func (s *operatorMessageCommitSink) Commit(event journal.CommittedEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *operatorMessageCommitSink) bodies() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var bodies []byte
	for _, event := range s.events {
		bodies = append(bodies, event.Body...)
	}
	return bodies
}

func TestDaemonOperatorMessageAuthorizesAndAttributesScopes(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedOperatorMessageRun(t, layout, crossRunTestGaggle, "target-run")
	service := newDaemonRunJournalService(layout, nil)

	accepted, err := service.SubmitOperatorMessage(context.Background(), operatorMessageRequest("target-run", "key-operate",
		httpapi.Principal{Subject: "operator", Issuer: "issuer", Roles: []httpapi.Role{httpapi.RoleOperate}}))
	if err != nil {
		t.Fatalf("operate submit: %v", err)
	}
	if !accepted.Accepted || accepted.Record.Request.PrincipalRef != "issuer:operator" {
		t.Fatalf("accepted = %+v", accepted)
	}

	denied, err := service.SubmitOperatorMessage(context.Background(), operatorMessageRequest("target-run", "key-view",
		httpapi.Principal{Subject: "viewer", Issuer: "issuer", Roles: []httpapi.Role{httpapi.RoleView}}))
	if err == nil {
		t.Fatal("view principal was accepted")
	}
	if !isInterventionStatus(err, http.StatusForbidden) || denied.Accepted ||
		denied.Record.Outcome == nil || denied.Record.Outcome.Code != operatorMessageDeniedCode ||
		denied.Record.Request.PrincipalRef != "issuer:viewer" {
		t.Fatalf("denied = %+v err = %v", denied, err)
	}

	podAccepted, err := service.SubmitOperatorMessage(context.Background(), operatorMessageRequest("target-run", "key-pod",
		httpapi.Principal{Subject: "run:target-run", Issuer: httpapi.PodPrincipalIssuer, Scopes: []string{httpapi.ScopeJournal}}))
	if err != nil {
		t.Fatalf("same-run pod submit: %v", err)
	}
	if !podAccepted.Accepted {
		t.Fatalf("pod accepted = %+v", podAccepted)
	}

	podDenied, err := service.SubmitOperatorMessage(context.Background(), operatorMessageRequest("target-run", "key-foreign-pod",
		httpapi.Principal{Subject: "run:other-run", Issuer: httpapi.PodPrincipalIssuer, Scopes: []string{httpapi.ScopeJournal}}))
	if err == nil || !isInterventionStatus(err, http.StatusForbidden) || podDenied.Record.Outcome == nil {
		t.Fatalf("foreign pod denied = %+v err = %v", podDenied, err)
	}

	reader, err := journal.OpenRead(filepath.Join(layout.ForGaggle(crossRunTestGaggle).RunsDir(), "target-run"))
	if err != nil {
		t.Fatal(err)
	}
	records, err := reader.OperatorMessages()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 {
		t.Fatalf("records = %d, want accepted and denied attempts: %+v", len(records), records)
	}
	refs := make([]string, 0, len(records))
	for _, record := range records {
		refs = append(refs, record.Request.PrincipalRef)
	}
	for _, want := range []string{"issuer:operator", "issuer:viewer", "goobers/pod:run:target-run", "goobers/pod:run:other-run"} {
		if !slices.Contains(refs, want) {
			t.Fatalf("principal refs = %v, missing %s", refs, want)
		}
	}
}

func TestDaemonOperatorMessageContentBoundary(t *testing.T) {
	secret := "ghp_" + strings.Repeat("e", 36)
	tests := []struct {
		name      string
		configure func(*httpapi.OperatorMessageSubmissionRequest)
		accepted  bool
	}{
		{
			name: "secret-bearing inline content and metadata",
			configure: func(request *httpapi.OperatorMessageSubmissionRequest) {
				request.IdempotencyKey = "key-" + secret
				request.Principal = httpapi.Principal{Subject: "operator-" + secret, Roles: []httpapi.Role{httpapi.RoleOperate}}
				request.PrincipalRef = request.Principal.Subject
				request.TargetAddress = "terminal:" + secret
				request.Purpose = "review-" + secret
				request.Content.Text = "Authorization: Bearer " + secret
				request.DeliveryMode = "terminal-" + secret
			},
			accepted: true,
		},
		{
			name: "oversized artifact reference",
			configure: func(request *httpapi.OperatorMessageSubmissionRequest) {
				request.Content = apiv1.OperatorMessageContent{Artifact: &apiv1.ArtifactPointer{
					Path: "artifacts/operator-message.txt", Digest: apiv1.Digest([]byte("content")),
					Size: apiv1.MaxOperatorMessageContentBytes + 1,
				}}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			layout := crossRunTestLayout(t)
			sink := &operatorMessageCommitSink{}
			unregister, err := journal.RegisterCommittedEventSink(layout.Root, "test-instance", sink)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(unregister)
			seedOperatorMessageRun(t, layout, crossRunTestGaggle, "boundary-run")
			service := newDaemonRunJournalService(layout, nil)
			request := operatorMessageRequest("boundary-run", "boundary-key",
				httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}})
			tc.configure(&request)

			response, err := service.SubmitOperatorMessage(context.Background(), request)
			runDir := filepath.Join(layout.ForGaggle(crossRunTestGaggle).RunsDir(), "boundary-run")
			if !tc.accepted {
				if err == nil {
					t.Fatal("SubmitOperatorMessage accepted unsafe input")
				}
				reader, openErr := journal.OpenRead(runDir)
				if openErr != nil {
					t.Fatal(openErr)
				}
				records, readErr := reader.OperatorMessages()
				if readErr != nil {
					t.Fatal(readErr)
				}
				if len(records) != 0 {
					t.Fatalf("rejected input produced a durable record: %#v", records)
				}
				return
			}
			if err != nil {
				t.Fatalf("SubmitOperatorMessage: %v", err)
			}
			assertOperatorMessageScrubbedSurfaces(t, runDir, sink, response, secret)
		})
	}
}

func assertOperatorMessageScrubbedSurfaces(t *testing.T, runDir string, sink *operatorMessageCommitSink, response httpapi.OperatorMessageSubmissionResponse, secret string) {
	t.Helper()
	if !response.Accepted || response.Record.State != apiv1.OperatorMessageAccepted {
		t.Fatalf("response = %#v", response)
	}
	recordJSON, err := json.Marshal(response.Record)
	if err != nil {
		t.Fatal(err)
	}
	assertOperatorMessageRepresentationScrubbed(t, "service response", recordJSON, secret)
	if response.Record.Request.RequestID == "" || response.Record.Request.IdempotencyKey == "" ||
		response.Record.Request.TargetAddress == "" || response.Record.Request.PrincipalRef == "" ||
		response.Record.Request.Purpose == "" || response.Record.Request.DeliveryMode == "" {
		t.Fatalf("scrubbing removed lifecycle metadata: %#v", response.Record.Request)
	}

	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := reader.OperatorMessages()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("operator message records = %d, want 1", len(records))
	}
	journalJSON, err := json.Marshal(records[0])
	if err != nil {
		t.Fatal(err)
	}
	assertOperatorMessageRepresentationScrubbed(t, "journal", journalJSON, secret)
	assertOperatorMessageRepresentationScrubbed(t, "telemetry commit", sink.bodies(), secret)

	run, _, err := journal.Recover(runDir)
	if err != nil {
		t.Fatal(err)
	}
	transcriptRef, err := run.RecordSpan("operator-message", "transcript", recordJSON)
	if err != nil {
		_ = run.Close()
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err = journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := reader.SpanBytes(transcriptRef)
	if err != nil {
		t.Fatal(err)
	}
	assertOperatorMessageRepresentationScrubbed(t, "transcript", transcript, secret)

	payload, err := converter.GetDefaultDataConverter().ToPayload(response.Record)
	if err != nil {
		t.Fatal(err)
	}
	assertOperatorMessageRepresentationScrubbed(t, "Temporal payload", payload.Data, secret)
	var temporalRecord apiv1.OperatorMessageRecord
	if err := converter.GetDefaultDataConverter().FromPayload(payload, &temporalRecord); err != nil {
		t.Fatal(err)
	}
	if temporalRecord.State != apiv1.OperatorMessageAccepted ||
		temporalRecord.Request.IdempotencyKey != response.Record.Request.IdempotencyKey {
		t.Fatalf("Temporal round trip lost lifecycle metadata: %#v", temporalRecord)
	}
}

func assertOperatorMessageRepresentationScrubbed(t *testing.T, name string, representation []byte, secret string) {
	t.Helper()
	if strings.Contains(string(representation), secret) {
		t.Fatalf("%s exposed raw secret", name)
	}
	if !strings.Contains(string(representation), journal.RedactedToken) &&
		!strings.Contains(string(representation), journal.Redacted) {
		t.Fatalf("%s did not retain redaction evidence: %s", name, representation)
	}
}

func TestDaemonOperatorMessageRefusesGaggleMismatch(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedOperatorMessageRun(t, layout, crossRunTestGaggle, "target-run")
	service := newDaemonRunJournalService(layout, nil)
	_, err := service.SubmitOperatorMessage(context.Background(), operatorMessageRequest("target-run", "key-gaggle",
		httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}}))
	if err != nil {
		t.Fatalf("baseline submit: %v", err)
	}
	mismatched := operatorMessageRequest("target-run", "key-wrong-gaggle",
		httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}})
	mismatched.Gaggle = "other-gaggle"
	if _, err := service.SubmitOperatorMessage(context.Background(), mismatched); err == nil || !isInterventionStatus(err, http.StatusForbidden) {
		t.Fatalf("gaggle mismatch err = %v", err)
	}
	reader, err := journal.OpenRead(filepath.Join(layout.ForGaggle(crossRunTestGaggle).RunsDir(), "target-run"))
	if err != nil {
		t.Fatal(err)
	}
	records, err := reader.OperatorMessages()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, record := range records {
		if record.Request.IdempotencyKey != "key-wrong-gaggle" {
			continue
		}
		found = true
		if record.Outcome == nil || record.Outcome.Status != apiv1.OperatorMessageRejected ||
			record.Request.PrincipalRef != "operator" {
			t.Fatalf("wrong-gaggle denial record = %+v", record)
		}
	}
	if !found {
		t.Fatalf("wrong-gaggle denial was not journaled: %+v", records)
	}
}

func TestOperatorMessageSubmissionDoesNotMutateInvocationAuthority(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedOperatorMessageRun(t, layout, crossRunTestGaggle, "target-run")
	service := newDaemonRunJournalService(layout, nil)
	endpoint, _ := fakeBlobPlane(t)
	digest := persistInvocationEnvelope(t, endpoint, apiv1.InvocationEnvelope{
		RunID: "target-run", TaskID: "implement", Gaggle: crossRunTestGaggle,
		Capabilities: []string{"repo:read"}, PolicyActions: []string{"issues:comment"},
		ParentPlatformPolicy: &apiv1.PlatformPolicy{
			Capabilities: []string{"repo:read"}, PolicyActions: []string{"issues:comment"},
			Credentials: []string{"repo-token"}, Sandbox: "workspace", FilesystemRoots: []string{"workspace"},
			NetworkEgress: []string{"github"}, Budget: apiv1.Limits{MaxTokens: 1000, MaxCostUSD: 1.25},
			Cancellation: "run", CompletionContract: "result",
		},
		Limits: apiv1.Limits{MaxDurationSeconds: 300, MaxTokens: 1000, MaxCostUSD: 1.25},
		NestedAgentPolicy: &apiv1.NestedAgentPolicy{
			Version: apiv1.NestedAgentPolicyVersion, Delegation: apiv1.DelegationBounded, MaxDepth: 1,
			PermittedProfiles: []string{"worker"},
			Context:           apiv1.NestedContextPolicy{Mode: apiv1.ContextExplicit, EnvelopeSections: []string{"goal"}},
			PlatformPolicy: apiv1.PlatformPolicy{
				Capabilities: []string{"repo:read"}, PolicyActions: []string{"issues:comment"},
				Credentials: []string{"repo-token"}, Sandbox: "workspace", FilesystemRoots: []string{"workspace"},
				NetworkEgress: []string{"github"}, Budget: apiv1.Limits{MaxTokens: 250, MaxCostUSD: 0.50},
				Cancellation: "run", CompletionContract: "result",
			},
		},
	})
	before := persistedEnvelopeAuthority(t, endpoint, digest)
	request := operatorMessageRequest("target-run", "key-authority-data",
		httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}})
	request.Content.Text = `{"capabilities":["repo:admin"],"credentials":["root"],"sandbox":"none","egress":["internet"],"budget":{"maxCostUSD":999},"delegation":"unbounded"}`
	if _, err := service.SubmitOperatorMessage(context.Background(), request); err != nil {
		t.Fatalf("submit message carrying authority-looking data: %v", err)
	}
	after := persistedEnvelopeAuthority(t, endpoint, digest)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("authority changed: before=%+v after=%+v", before, after)
	}
}

func TestDaemonOperatorMessageDeliversLiveBetweenTurnToSelectedAgentOnly(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedOperatorMessageRun(t, layout, crossRunTestGaggle, "target-run")
	addressA := operatorMessageAgentAddress(t, "target-run", "implement", 1, "agent-a", 2)
	addressB := operatorMessageAgentAddress(t, "target-run", "implement", 1, "agent-b", 2)
	targetA := registerOperatorMessageTarget(t, addressA, []string{invoke.OperatorMessageModeBetweenTurn}, nil)
	targetB := registerOperatorMessageTarget(t, addressB, []string{invoke.OperatorMessageModeBetweenTurn}, nil)
	service := newDaemonRunJournalService(layout, nil)

	request := operatorMessageRequest("target-run", "key-live",
		httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}})
	request.TargetAddress = addressA
	record, err := service.SubmitOperatorMessage(context.Background(), request)
	if err != nil {
		t.Fatalf("submit live message: %v", err)
	}
	if !record.Accepted || record.Record.Request.DeliveryMode != invoke.OperatorMessageModeBetweenTurn ||
		record.Record.Outcome == nil || record.Record.Outcome.Status != apiv1.OperatorMessageDelivered {
		t.Fatalf("live delivery record = %+v", record)
	}
	if len(targetA.deliveries) != 1 || targetA.deliveries[0].TargetAddress != addressA ||
		targetA.deliveries[0].Message.Content.Text != "please review" {
		t.Fatalf("target A deliveries = %+v", targetA.deliveries)
	}
	if len(targetB.deliveries) != 0 {
		t.Fatalf("sibling target received delivery: %+v", targetB.deliveries)
	}
}

func TestDaemonOperatorMessageDeliversNestedSelectionThroughStageTarget(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedOperatorMessageRun(t, layout, crossRunTestGaggle, "target-run")
	startedSeq := seedOperatorMessageLiveAgent(t, layout, crossRunTestGaggle, "target-run", "implement", 1, "worker-child")
	stageAddress := operatorMessageAgentAddress(t, "target-run", "implement", 1, "coder", startedSeq)
	nestedAddress := operatorMessageAgentAddress(t, "target-run", "implement", 1, "worker-child", startedSeq)
	target := registerOperatorMessageTarget(t, stageAddress, []string{invoke.OperatorMessageModeBetweenTurn}, nil)
	service := newDaemonRunJournalService(layout, nil)

	request := operatorMessageRequest("target-run", "key-nested-live",
		httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}})
	request.TargetAddress = nestedAddress
	record, err := service.SubmitOperatorMessage(context.Background(), request)
	if err != nil {
		t.Fatalf("submit nested live message: %v", err)
	}
	if !record.Accepted || record.Record.Request.DeliveryMode != invoke.OperatorMessageModeBetweenTurn ||
		record.Record.Outcome == nil || record.Record.Outcome.Status != apiv1.OperatorMessageDelivered {
		t.Fatalf("nested live delivery record = %+v", record)
	}
	if len(target.deliveries) != 1 || target.deliveries[0].TargetAddress != nestedAddress {
		t.Fatalf("stage target deliveries = %+v", target.deliveries)
	}
}

func TestDaemonOperatorMessageRetryResumesAcceptedLiveDelivery(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedOperatorMessageRun(t, layout, crossRunTestGaggle, "target-run")
	startedSeq := seedOperatorMessageLiveAgent(t, layout, crossRunTestGaggle, "target-run", "implement", 1, "agent-a")
	address := operatorMessageAgentAddress(t, "target-run", "implement", 1, "agent-a", startedSeq)
	request := acceptedOperatorMessageRequest("key-resume-accepted", address, invoke.OperatorMessageModeBetweenTurn)
	seedAcceptedOperatorMessage(t, layout, crossRunTestGaggle, "target-run", request, false)
	target := registerOperatorMessageTarget(t, address, []string{invoke.OperatorMessageModeBetweenTurn}, nil)
	service := newDaemonRunJournalService(layout, nil)

	submission := operatorMessageRequest("target-run", "key-resume-accepted",
		httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}})
	submission.TargetAddress = address
	record, err := service.SubmitOperatorMessage(context.Background(), submission)
	if err != nil {
		t.Fatalf("retry accepted live message: %v", err)
	}
	if record.Accepted || record.Record.Outcome == nil ||
		record.Record.Outcome.Status != apiv1.OperatorMessageDelivered ||
		len(target.deliveries) != 1 {
		t.Fatalf("resumed accepted record = %+v deliveries = %+v", record, target.deliveries)
	}
}

func TestDaemonOperatorMessageRetryCompletesAcknowledgedLiveDeliveryWithoutRedelivery(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedOperatorMessageRun(t, layout, crossRunTestGaggle, "target-run")
	startedSeq := seedOperatorMessageLiveAgent(t, layout, crossRunTestGaggle, "target-run", "implement", 1, "agent-a")
	address := operatorMessageAgentAddress(t, "target-run", "implement", 1, "agent-a", startedSeq)
	request := acceptedOperatorMessageRequest("key-resume-ack", address, invoke.OperatorMessageModeBetweenTurn)
	seedAcceptedOperatorMessage(t, layout, crossRunTestGaggle, "target-run", request, true)
	target := registerOperatorMessageTarget(t, address, []string{invoke.OperatorMessageModeBetweenTurn}, nil)
	service := newDaemonRunJournalService(layout, nil)

	submission := operatorMessageRequest("target-run", "key-resume-ack",
		httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}})
	submission.TargetAddress = address
	record, err := service.SubmitOperatorMessage(context.Background(), submission)
	if err != nil {
		t.Fatalf("retry acknowledged live message: %v", err)
	}
	if record.Accepted || record.Record.Outcome == nil ||
		record.Record.Outcome.Status != apiv1.OperatorMessageDelivered ||
		len(target.deliveries) != 0 {
		t.Fatalf("resumed acknowledged record = %+v deliveries = %+v", record, target.deliveries)
	}
}

func TestDaemonOperatorMessageLocalRunnerLifecycle(t *testing.T) {
	t.Run("between-turn delivery retry termination and continuation", func(t *testing.T) {
		layout := crossRunTestLayout(t)
		runID := "run-live-between-turn"
		reviewer := newBlockingOperatorMessageReviewer([]string{invoke.OperatorMessageModeBetweenTurn}, nil)
		runDone := startOperatorMessageRunner(t, layout, runID, reviewer)
		address := waitForOperatorMessageGateAddress(t, layout, runID, "reviewer")
		service := newDaemonRunJournalService(layout, nil)

		request := operatorMessageRequest(runID, "key-live-runner",
			httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}})
		request.TargetAddress = address
		record, err := service.SubmitOperatorMessage(context.Background(), request)
		if err != nil {
			t.Fatalf("submit live runner message: %v", err)
		}
		if !record.Accepted || record.Record.Request.DeliveryMode != invoke.OperatorMessageModeBetweenTurn ||
			record.Record.Outcome == nil || record.Record.Outcome.Status != apiv1.OperatorMessageDelivered {
			t.Fatalf("live runner record = %+v", record)
		}
		delivery := reviewer.waitForDelivery(t)
		if delivery.TargetAddress != address || delivery.Message.IdempotencyKey != "key-live-runner" {
			t.Fatalf("delivery = %+v", delivery)
		}
		record, err = service.SubmitOperatorMessage(context.Background(), request)
		if err != nil {
			t.Fatalf("retry live runner message: %v", err)
		}
		if record.Accepted || reviewer.deliveryCount() != 1 {
			t.Fatalf("retry accepted=%v delivery count=%d", record.Accepted, reviewer.deliveryCount())
		}

		reviewer.release()
		waitForOperatorMessageRunDone(t, runDone)
		if _, ok := runner.DefaultOperatorMessageDeliveryRegistry.Resolve(address); ok {
			t.Fatal("terminated reviewer retained its live delivery target")
		}
		if _, ok := runner.DefaultOperatorMessageDeliveryRegistry.ResolveJournal(address); ok {
			t.Fatal("terminated reviewer retained its live journal registration")
		}

		request = operatorMessageRequest(runID, "key-after-termination",
			httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}})
		request.TargetAddress = address
		record, err = service.SubmitOperatorMessage(context.Background(), request)
		if err != nil {
			t.Fatalf("submit after termination: %v", err)
		}
		if !record.Accepted || record.Record.Request.DeliveryMode != invoke.OperatorMessageModeNextAttempt ||
			record.Record.Outcome != nil {
			t.Fatalf("terminated target record = %+v", record)
		}
	})

	t.Run("interrupt cancellation and delivery", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			runID      string
			deliverErr error
			wantStatus apiv1.OperatorMessageOutcomeStatus
			wantCode   string
		}{
			{name: "canceled", runID: "run-live-interrupt-cancel", deliverErr: context.Canceled, wantStatus: apiv1.OperatorMessageFailed, wantCode: "delivery_canceled"},
			{name: "delivered", runID: "run-live-interrupt-deliver", wantStatus: apiv1.OperatorMessageDelivered},
		} {
			t.Run(tc.name, func(t *testing.T) {
				layout := crossRunTestLayout(t)
				reviewer := newBlockingOperatorMessageReviewer([]string{invoke.OperatorMessageModeInterruptAndContinue}, tc.deliverErr)
				runDone := startOperatorMessageRunner(t, layout, tc.runID, reviewer)
				address := waitForOperatorMessageGateAddress(t, layout, tc.runID, "reviewer")
				service := newDaemonRunJournalService(layout, nil)

				request := operatorMessageRequest(tc.runID, "key-"+tc.name,
					httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}})
				request.TargetAddress = address
				record, err := service.SubmitOperatorMessage(context.Background(), request)
				if err != nil {
					t.Fatalf("submit interrupt message: %v", err)
				}
				if !record.Accepted || record.Record.Request.DeliveryMode != invoke.OperatorMessageModeInterruptAndContinue ||
					record.Record.Outcome == nil || record.Record.Outcome.Status != tc.wantStatus ||
					record.Record.Outcome.Code != tc.wantCode {
					t.Fatalf("interrupt record = %+v", record)
				}
				_ = reviewer.waitForDelivery(t)
				reviewer.release()
				waitForOperatorMessageRunDone(t, runDone)
			})
		}
	})
}

func TestDaemonOperatorMessageRejectsForeignRunAgentAddress(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedOperatorMessageRun(t, layout, crossRunTestGaggle, "target-run")
	service := newDaemonRunJournalService(layout, nil)
	request := operatorMessageRequest("target-run", "key-foreign-address",
		httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}})
	request.TargetAddress = operatorMessageAgentAddress(t, "other-run", "implement", 1, "agent-a", 2)

	if _, err := service.SubmitOperatorMessage(context.Background(), request); !isInterventionStatus(err, http.StatusBadRequest) {
		t.Fatalf("foreign target address error = %v, want bad request", err)
	}
}

func TestDaemonOperatorMessageDeliversInterruptAndContinue(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedOperatorMessageRun(t, layout, crossRunTestGaggle, "target-run")
	addressInterrupt := operatorMessageAgentAddress(t, "target-run", "implement", 1, "agent-interrupt", 2)
	interruptTarget := registerOperatorMessageTarget(t, addressInterrupt, []string{invoke.OperatorMessageModeInterruptAndContinue}, nil)
	service := newDaemonRunJournalService(layout, nil)

	request := operatorMessageRequest("target-run", "key-interrupt",
		httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}})
	request.TargetAddress = addressInterrupt
	record, err := service.SubmitOperatorMessage(context.Background(), request)
	if err != nil {
		t.Fatalf("submit interrupt-mode message: %v", err)
	}
	if !record.Accepted || record.Record.Request.DeliveryMode != invoke.OperatorMessageModeInterruptAndContinue ||
		record.Record.Outcome == nil || record.Record.Outcome.Status != apiv1.OperatorMessageDelivered ||
		len(interruptTarget.deliveries) != 1 || interruptTarget.deliveries[0].TargetAddress != addressInterrupt {
		t.Fatalf("interrupt delivery record = %+v deliveries = %+v", record, interruptTarget.deliveries)
	}
}

func TestDaemonOperatorMessageRecordsNextAttemptWithoutLiveSuccess(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedOperatorMessageRun(t, layout, crossRunTestGaggle, "target-run")
	service := newDaemonRunJournalService(layout, nil)

	request := operatorMessageRequest("target-run", "key-next",
		httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}})
	request.TargetAddress = operatorMessageAgentAddress(t, "target-run", "implement", 1, "agent-missing", 2)
	record, err := service.SubmitOperatorMessage(context.Background(), request)
	if err != nil {
		t.Fatalf("submit next-attempt message: %v", err)
	}
	if !record.Accepted || record.Record.Request.DeliveryMode != invoke.OperatorMessageModeNextAttempt ||
		record.Record.Outcome != nil {
		t.Fatalf("next-attempt record = %+v", record)
	}
}

func TestDaemonOperatorMessageLiveDeliveryCancellationIsTypedAndIdempotent(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedOperatorMessageRun(t, layout, crossRunTestGaggle, "target-run")
	address := operatorMessageAgentAddress(t, "target-run", "implement", 1, "agent-a", 2)
	target := registerOperatorMessageTarget(t, address, []string{invoke.OperatorMessageModeBetweenTurn}, context.Canceled)
	service := newDaemonRunJournalService(layout, nil)

	request := operatorMessageRequest("target-run", "key-cancel",
		httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}})
	request.TargetAddress = address
	record, err := service.SubmitOperatorMessage(context.Background(), request)
	if err != nil {
		t.Fatalf("submit canceled message: %v", err)
	}
	if record.Record.Outcome == nil || record.Record.Outcome.Status != apiv1.OperatorMessageFailed ||
		record.Record.Outcome.Code != "delivery_canceled" {
		t.Fatalf("canceled delivery record = %+v", record)
	}
	record, err = service.SubmitOperatorMessage(context.Background(), request)
	if err != nil {
		t.Fatalf("retry canceled message: %v", err)
	}
	if record.Accepted || len(target.deliveries) != 1 {
		t.Fatalf("duplicate retry accepted=%v deliveries=%d", record.Accepted, len(target.deliveries))
	}
}

type invocationAuthoritySnapshot struct {
	envelope             apiv1.InvocationEnvelope
	Capabilities         []string
	PolicyActions        []string
	ParentPlatformPolicy *apiv1.PlatformPolicy
	Limits               apiv1.Limits
	NestedAgentPolicy    *apiv1.NestedAgentPolicy
}

func persistInvocationEnvelope(t *testing.T, endpoint string, env apiv1.InvocationEnvelope) string {
	t.Helper()
	data, digest, err := agentickit.Marshal(&agentickit.Kit{Envelope: env})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&dispatcher.BlobClient{BaseURL: endpoint, Token: "pod-token"}).Put(context.Background(), digest, data); err != nil {
		t.Fatal(err)
	}
	return digest
}

func persistedEnvelopeAuthority(t *testing.T, endpoint, digest string) invocationAuthoritySnapshot {
	t.Helper()
	data, err := (&dispatcher.BlobClient{BaseURL: endpoint, Token: "pod-token"}).Get(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	kit, err := agentickit.Unmarshal(data, digest)
	if err != nil {
		t.Fatal(err)
	}
	env := kit.Envelope
	return invocationAuthoritySnapshot{
		envelope:             env,
		Capabilities:         env.Capabilities,
		PolicyActions:        env.PolicyActions,
		ParentPlatformPolicy: env.ParentPlatformPolicy,
		Limits:               env.Limits,
		NestedAgentPolicy:    env.NestedAgentPolicy,
	}
}

func operatorMessageRequest(runID, key string, principal httpapi.Principal) httpapi.OperatorMessageSubmissionRequest {
	ref := principal.Subject
	if principal.Issuer != "" {
		ref = principal.Issuer + ":" + principal.Subject
	}
	return httpapi.OperatorMessageSubmissionRequest{
		OperatorMessageSubmitRequest: apicontract.OperatorMessageSubmitRequest{
			RunID: runID, IdempotencyKey: key, PrincipalRef: ref, Gaggle: crossRunTestGaggle,
			TargetAddress: "terminal:operator", Purpose: "approval-required",
			Content: apiv1.OperatorMessageContent{Text: "please review"}, DeliveryMode: "terminal",
		},
		Principal: principal,
	}
}

func isInterventionStatus(err error, status int) bool {
	var planeErr *httpapi.InterventionError
	return errors.As(err, &planeErr) && planeErr.Status == status
}

func seedOperatorMessageRun(t *testing.T, layout instance.Layout, gaggle, runID string) {
	t.Helper()
	run, err := journal.Create(layout.ForGaggle(gaggle).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
	}, nil)
	if err != nil {
		t.Fatalf("create run %s: %v", runID, err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close run %s: %v", runID, err)
	}
}

func seedOperatorMessageLiveAgent(t *testing.T, layout instance.Layout, gaggle, runID, stage string, attempt int, agentID string) uint64 {
	t.Helper()
	run, _, err := journal.Recover(filepath.Join(layout.ForGaggle(gaggle).RunsDir(), runID))
	if err != nil {
		t.Fatalf("recover run %s: %v", runID, err)
	}
	defer func() { _ = run.Close() }()
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: stage, Attempt: attempt}); err != nil {
		t.Fatalf("append stage start: %v", err)
	}
	startedSeq := run.Seq()
	now := time.Now().UTC()
	if err := run.Append(journal.Event{
		Type: journal.EventAgentLifecycle, Stage: stage, Attempt: attempt,
		Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: agentID, RunID: runID, Stage: stage, Attempt: attempt,
			Lifecycle: journal.AgentWaiting, StartedAt: now, UpdatedAt: now, Fidelity: journal.AgentFidelityFull,
		},
	}); err != nil {
		t.Fatalf("append agent lifecycle: %v", err)
	}
	return startedSeq
}

func acceptedOperatorMessageRequest(key, address, mode string) apiv1.OperatorMessageRequest {
	return apiv1.OperatorMessageRequest{
		Schema:         apiv1.OperatorMessageRequestSchema,
		RequestID:      key,
		IdempotencyKey: key,
		TargetAddress:  address,
		PrincipalRef:   "operator",
		RequestedAt:    time.Now().UTC(),
		Purpose:        "approval-required",
		Content:        apiv1.OperatorMessageContent{Text: "please review"},
		DeliveryMode:   mode,
	}
}

func seedAcceptedOperatorMessage(t *testing.T, layout instance.Layout, gaggle, runID string, request apiv1.OperatorMessageRequest, acknowledged bool) {
	t.Helper()
	run, _, err := journal.Recover(filepath.Join(layout.ForGaggle(gaggle).RunsDir(), runID))
	if err != nil {
		t.Fatalf("recover run %s: %v", runID, err)
	}
	defer func() { _ = run.Close() }()
	if _, accepted, err := run.AcceptOperatorMessage(request); err != nil || !accepted {
		t.Fatalf("AcceptOperatorMessage = accepted %v, err %v", accepted, err)
	}
	if acknowledged {
		if _, err := run.AcknowledgeOperatorMessage(operatorMessageAcknowledgement(request)); err != nil {
			t.Fatalf("AcknowledgeOperatorMessage: %v", err)
		}
	}
}

type fakeOperatorMessageTarget struct {
	modes      []string
	err        error
	deliveries []invoke.OperatorMessageDeliveryRequest
}

func (f *fakeOperatorMessageTarget) OperatorMessageDeliveryModes() []string {
	return append([]string(nil), f.modes...)
}

func (f *fakeOperatorMessageTarget) DeliverOperatorMessage(_ context.Context, req invoke.OperatorMessageDeliveryRequest) error {
	f.deliveries = append(f.deliveries, req)
	return f.err
}

func registerOperatorMessageTarget(t *testing.T, address string, modes []string, err error) *fakeOperatorMessageTarget {
	t.Helper()
	target := &fakeOperatorMessageTarget{modes: modes, err: err}
	unregister := runner.DefaultOperatorMessageDeliveryRegistry.RegisterWithJournal(address, target, nil)
	t.Cleanup(unregister)
	return target
}

type operatorMessageRunResult struct {
	result runner.Result
	err    error
}

type operatorMessageDeterministic struct{}

func (operatorMessageDeterministic) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

type blockingOperatorMessageReviewer struct {
	modes       []string
	err         error
	started     chan struct{}
	released    chan struct{}
	delivered   chan invoke.OperatorMessageDeliveryRequest
	startOnce   sync.Once
	releaseOnce sync.Once
	mu          sync.Mutex
	deliveries  []invoke.OperatorMessageDeliveryRequest
}

func newBlockingOperatorMessageReviewer(modes []string, err error) *blockingOperatorMessageReviewer {
	return &blockingOperatorMessageReviewer{
		modes:     modes,
		err:       err,
		started:   make(chan struct{}),
		released:  make(chan struct{}),
		delivered: make(chan invoke.OperatorMessageDeliveryRequest, 8),
	}
}

func (b *blockingOperatorMessageReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (b *blockingOperatorMessageReviewer) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	b.startOnce.Do(func() { close(b.started) })
	<-b.released
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func (b *blockingOperatorMessageReviewer) OperatorMessageDeliveryModes() []string {
	return append([]string(nil), b.modes...)
}

func (b *blockingOperatorMessageReviewer) DeliverOperatorMessage(_ context.Context, req invoke.OperatorMessageDeliveryRequest) error {
	b.mu.Lock()
	b.deliveries = append(b.deliveries, req)
	b.mu.Unlock()
	b.delivered <- req
	return b.err
}

func (b *blockingOperatorMessageReviewer) waitForDelivery(t *testing.T) invoke.OperatorMessageDeliveryRequest {
	t.Helper()
	select {
	case delivery := <-b.delivered:
		return delivery
	case <-time.After(15 * time.Second):
		t.Fatal("operator message was not delivered to live reviewer")
		return invoke.OperatorMessageDeliveryRequest{}
	}
}

func (b *blockingOperatorMessageReviewer) deliveryCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.deliveries)
}

func (b *blockingOperatorMessageReviewer) release() {
	b.releaseOnce.Do(func() { close(b.released) })
}

func startOperatorMessageRunner(t *testing.T, layout instance.Layout, runID string, reviewer *blockingOperatorMessageReviewer) <-chan operatorMessageRunResult {
	t.Helper()
	t.Cleanup(reviewer.release)
	wtMgr, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	r, err := runner.New(runner.Config{
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return operatorMessageDeterministic{}, nil
		},
		NewAgentic: func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) {
			return reviewer, nil
		},
		Worktrees:    wtMgr,
		RunsDir:      layout.ForGaggle(crossRunTestGaggle).RunsDir(),
		ScratchDir:   filepath.Join(t.TempDir(), "scratch"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return "", errors.New("repo workspace should not be provisioned") },
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	machine := operatorMessageAgenticGateMachine(t)
	done := make(chan operatorMessageRunResult, 1)
	go func() {
		result, startErr := r.Start(context.Background(), runner.StartInput{
			RunID:   runID,
			Machine: machine,
			Gaggle:  crossRunTestGaggle,
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		done <- operatorMessageRunResult{result: result, err: startErr}
	}()
	select {
	case <-reviewer.started:
	case <-time.After(15 * time.Second):
		t.Fatal("reviewer did not start")
	}
	return done
}

func waitForOperatorMessageRunDone(t *testing.T, done <-chan operatorMessageRunResult) {
	t.Helper()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("runner start: %v", got.err)
		}
		if got.result.Phase != journal.PhaseCompleted {
			t.Fatalf("runner phase = %q, want completed", got.result.Phase)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runner did not finish")
	}
}

func waitForOperatorMessageGateAddress(t *testing.T, layout instance.Layout, runID, agent string) string {
	t.Helper()
	runDir := filepath.Join(layout.ForGaggle(crossRunTestGaggle).RunsDir(), runID)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		reader, err := journal.OpenRead(runDir)
		if err == nil {
			events, err := reader.Events()
			if err == nil {
				for i := len(events) - 1; i >= 0; i-- {
					event := events[i]
					if event.Type != journal.EventReviewerStarted || event.Stage != "review" {
						continue
					}
					// Delivery belongs to this reviewer dispatch, not the enclosing
					// gate visit: retries each have their own durable start identity.
					address, err := journal.StageAgentAddress(runID, event.Stage, event.Attempt, agent, event.Seq)
					if err != nil {
						t.Fatal(err)
					}
					if _, ok := runner.DefaultOperatorMessageDeliveryRegistry.Resolve(address.String()); !ok {
						t.Fatal("reviewer dispatch has no live delivery target")
					}
					if _, ok := runner.DefaultOperatorMessageDeliveryRegistry.ResolveJournal(address.String()); !ok {
						t.Fatal("reviewer dispatch has no live journal registration")
					}
					return address.String()
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("reviewer dispatch address was not journaled")
	return ""
}

func operatorMessageAgenticGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   crossRunTestGaggle,
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a result", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "review"},
		},
		Gates: []apiv1.Gate{{
			Name:      "review",
			Evaluator: apiv1.EvaluatorAgentic,
			Agentic:   &apiv1.AgenticGate{Goober: "reviewer", Workspace: apiv1.WorkspaceScratch},
			Branches:  map[string]string{"pass": workflow.TerminalComplete, "needs-changes": "implement", "fail": workflow.TargetAbort},
		}},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "operator-message-local-runner", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile operator-message machine: %v", err)
	}
	return machine
}

func operatorMessageAgentAddress(t *testing.T, runID, stage string, attempt int, agent string, startedSeq uint64) string {
	t.Helper()
	address, err := journal.StageAgentAddress(runID, stage, attempt, agent, startedSeq)
	if err != nil {
		t.Fatal(err)
	}
	return address.String()
}
