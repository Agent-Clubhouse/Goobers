package journal

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestOperatorMessageLifecycleReplaysAfterRecovery(t *testing.T) {
	run, root := newRun(t)
	request := testOperatorMessageRequest("request-1", "key-1")

	record, accepted, err := run.AcceptOperatorMessage(request)
	if err != nil {
		t.Fatalf("AcceptOperatorMessage: %v", err)
	}
	if !accepted || record.State != apiv1.OperatorMessageAccepted {
		t.Fatalf("accept = (%v, %q), want (true, accepted)", accepted, record.State)
	}

	duplicate := request
	duplicate.RequestID = "request-duplicate"
	record, accepted, err = run.AcceptOperatorMessage(duplicate)
	if err != nil {
		t.Fatalf("duplicate AcceptOperatorMessage: %v", err)
	}
	if accepted || record.Request.RequestID != request.RequestID {
		t.Fatalf("duplicate = (%v, %q), want original request %q", accepted, record.Request.RequestID, request.RequestID)
	}

	ack := apiv1.OperatorMessageAcknowledgement{
		Schema:         apiv1.OperatorMessageAcknowledgementSchema,
		RequestID:      request.RequestID,
		IdempotencyKey: request.IdempotencyKey,
		PrincipalRef:   "operator:acknowledger",
		AcknowledgedAt: fixedClock()().Add(time.Minute),
	}
	record, err = run.AcknowledgeOperatorMessage(ack)
	if err != nil {
		t.Fatalf("AcknowledgeOperatorMessage: %v", err)
	}
	if record.State != apiv1.OperatorMessageAcknowledged || record.Acknowledgement == nil {
		t.Fatalf("acknowledged record = %#v", record)
	}

	outcome := apiv1.OperatorMessageOutcome{
		Schema:         apiv1.OperatorMessageOutcomeSchema,
		RequestID:      request.RequestID,
		IdempotencyKey: request.IdempotencyKey,
		CompletedAt:    fixedClock()().Add(2 * time.Minute),
		Status:         apiv1.OperatorMessageDelivered,
	}
	record, err = run.CompleteOperatorMessage(outcome)
	if err != nil {
		t.Fatalf("CompleteOperatorMessage: %v", err)
	}
	if record.State != apiv1.OperatorMessageState(apiv1.OperatorMessageDelivered) ||
		record.Acknowledgement == nil || record.Outcome == nil {
		t.Fatalf("terminal record = %#v", record)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reader, err := OpenRead(filepath.Join(root, testIdentity().RunID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	records, err := reader.OperatorMessages()
	if err != nil {
		t.Fatalf("OperatorMessages: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	got := records[0]
	if got.Request.TargetAddress != request.TargetAddress ||
		got.Request.PrincipalRef != request.PrincipalRef ||
		got.Request.DeliveryMode != request.DeliveryMode ||
		got.State != apiv1.OperatorMessageState(apiv1.OperatorMessageDelivered) ||
		got.Acknowledgement == nil {
		t.Fatalf("replayed record = %#v", got)
	}
}

func TestOperatorMessageExpiredRequestIsTypedAndIdempotent(t *testing.T) {
	run, _ := newRun(t)
	defer func() { _ = run.Close() }()
	request := testOperatorMessageRequest("expired-1", "expired-key")
	expires := fixedClock()().Add(-time.Second)
	request.ExpiresAt = &expires

	record, accepted, err := run.AcceptOperatorMessage(request)
	if err != nil {
		t.Fatalf("AcceptOperatorMessage: %v", err)
	}
	if accepted || record.State != apiv1.OperatorMessageState(apiv1.OperatorMessageExpired) ||
		record.Outcome == nil || record.Outcome.Code != "request_expired" {
		t.Fatalf("expired record = %#v, accepted = %v", record, accepted)
	}

	duplicate := request
	duplicate.RequestID = "expired-duplicate"
	record, accepted, err = run.AcceptOperatorMessage(duplicate)
	if err != nil {
		t.Fatalf("duplicate AcceptOperatorMessage: %v", err)
	}
	if accepted || record.Request.RequestID != request.RequestID {
		t.Fatalf("duplicate expired request resolved to %#v, accepted = %v", record, accepted)
	}
	reader, err := OpenRead(run.Dir())
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	count := 0
	for _, event := range events {
		if event.Type == EventOperatorMessageOutcome {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("outcome events = %d, want 1", count)
	}
}

func TestOperatorMessageRejectedAndArtifactContentReplay(t *testing.T) {
	run, _ := newRun(t)
	defer func() { _ = run.Close() }()
	request := testOperatorMessageRequest("artifact-1", "artifact-key")
	request.Content = apiv1.OperatorMessageContent{Artifact: &apiv1.ArtifactPointer{
		Path:      "artifacts/messages/body.txt",
		Digest:    apiv1.Digest([]byte("body")),
		MediaType: "text/plain",
		Size:      4,
	}}
	if _, accepted, err := run.AcceptOperatorMessage(request); err != nil || !accepted {
		t.Fatalf("AcceptOperatorMessage = accepted %v, err %v", accepted, err)
	}
	record, err := run.CompleteOperatorMessage(apiv1.OperatorMessageOutcome{
		Schema:         apiv1.OperatorMessageOutcomeSchema,
		RequestID:      request.RequestID,
		IdempotencyKey: request.IdempotencyKey,
		CompletedAt:    fixedClock()().Add(time.Minute),
		Status:         apiv1.OperatorMessageRejected,
		Code:           "target_refused",
		Detail:         "target does not accept this purpose",
	})
	if err != nil {
		t.Fatalf("CompleteOperatorMessage: %v", err)
	}
	if record.State != apiv1.OperatorMessageState(apiv1.OperatorMessageRejected) ||
		record.Request.Content.Artifact == nil || record.Request.Content.Artifact.Digest != request.Content.Artifact.Digest {
		t.Fatalf("rejected artifact record = %#v", record)
	}
}

func TestOperatorMessageJournalScrubsInlineContent(t *testing.T) {
	run, _ := newRun(t)
	defer func() { _ = run.Close() }()
	request := testOperatorMessageRequest("secret-1", "secret-key")
	request.Content.Text = "Authorization: Bearer ghp_abcdefghijklmnopqrstuvwxyz1234567890"
	if _, _, err := run.AcceptOperatorMessage(request); err != nil {
		t.Fatalf("AcceptOperatorMessage: %v", err)
	}
	reader, err := OpenRead(run.Dir())
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	records, err := reader.OperatorMessages()
	if err != nil {
		t.Fatalf("OperatorMessages: %v", err)
	}
	if strings.Contains(records[0].Request.Content.Text, "ghp_") ||
		records[0].Request.Content.Text == request.Content.Text {
		t.Fatalf("persisted content was not scrubbed: %q", records[0].Request.Content.Text)
	}
}

func TestOperatorMessageValidationAndTerminalGuards(t *testing.T) {
	request := testOperatorMessageRequest("invalid-1", "invalid-key")
	request.Content = apiv1.OperatorMessageContent{}
	if err := request.Validate(); err == nil {
		t.Fatal("Validate accepted missing content")
	}
	request = testOperatorMessageRequest("large-1", "large-key")
	request.Content = apiv1.OperatorMessageContent{Artifact: &apiv1.ArtifactPointer{
		Path: "artifacts/large", Digest: apiv1.Digest([]byte("large")),
		Size: apiv1.MaxOperatorMessageContentBytes + 1,
	}}
	if err := request.Validate(); err == nil {
		t.Fatal("Validate accepted oversized artifact")
	}

	run, _ := newRun(t)
	defer func() { _ = run.Close() }()
	if _, err := run.AcknowledgeOperatorMessage(apiv1.OperatorMessageAcknowledgement{
		Schema: apiv1.OperatorMessageAcknowledgementSchema, RequestID: "missing",
		IdempotencyKey: "missing", PrincipalRef: "operator:x", AcknowledgedAt: fixedClock()(),
	}); !errors.Is(err, ErrOperatorMessageNotFound) {
		t.Fatalf("acknowledge missing error = %v", err)
	}
}

func testOperatorMessageRequest(requestID, key string) apiv1.OperatorMessageRequest {
	expires := fixedClock()().Add(time.Hour)
	return apiv1.OperatorMessageRequest{
		Schema:         apiv1.OperatorMessageRequestSchema,
		RequestID:      requestID,
		IdempotencyKey: key,
		TargetAddress:  "terminal:operator",
		PrincipalRef:   "user:requester",
		RequestedAt:    fixedClock()(),
		ExpiresAt:      &expires,
		Purpose:        "approval-required",
		Content:        apiv1.OperatorMessageContent{Text: "Please review the run."},
		DeliveryMode:   "terminal",
	}
}
