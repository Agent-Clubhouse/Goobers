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

	"go.temporal.io/sdk/converter"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
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
