package notification

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

type memoryRecorder struct {
	mu       sync.Mutex
	requests []apiv1.NotificationRequest
	receipts []apiv1.NotificationReceipt
}

func (r *memoryRecorder) RecordRequest(_ context.Context, request apiv1.NotificationRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, request)
	return nil
}

func (r *memoryRecorder) RecordReceipt(_ context.Context, receipt apiv1.NotificationReceipt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.receipts = append(r.receipts, receipt)
	return nil
}

func (r *memoryRecorder) ClaimDelivery(_ context.Context, pending apiv1.NotificationReceipt) (apiv1.NotificationReceipt, RecordedDeliveryState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.receipts) - 1; i >= 0; i-- {
		receipt := r.receipts[i]
		if receipt.IdempotencyDigest != pending.IdempotencyDigest || receipt.Sink.Kind != pending.Sink.Kind || receipt.Attempt == 0 {
			continue
		}
		switch {
		case receipt.Status == apiv1.NotificationDelivered:
			return receipt, DeliveryComplete, nil
		case receipt.Status == apiv1.NotificationPending || receipt.Unresolved:
			return receipt, DeliveryUnresolved, nil
		default:
			r.receipts = append(r.receipts, pending)
			return apiv1.NotificationReceipt{}, DeliveryClaimed, nil
		}
	}
	r.receipts = append(r.receipts, pending)
	return apiv1.NotificationReceipt{}, DeliveryClaimed, nil
}

func validRequest(sinks ...string) apiv1.NotificationRequest {
	return apiv1.NotificationRequest{
		Schema: apiv1.NotificationRequestSchema, NotificationID: "notice-1",
		IncidentID: "incident-1", EventID: "event-1",
		Severity: apiv1.NotificationSeverityCritical, Transition: "opened",
		Title: "exact title", Body: "exact body", SpeechText: "exact speech",
		Facts:    []apiv1.NotificationFact{{Name: "host", Value: "worker-1"}},
		Evidence: []apiv1.NotificationEvidenceRef{{Kind: "artifact", ID: "evidence-1"}},
		Source:   apiv1.NotificationSource{RunID: "0123456789abcdef0123456789abcdef", Workflow: "mission-control", Stage: "decide"},
		Sinks:    sinks, ExpiresAt: time.Now().Add(time.Hour), IdempotencyKey: "incident-1:opened",
	}
}

func newTestDispatcher(t *testing.T, recorder Recorder, policy Policy, sinks ...Sink) *Dispatcher {
	t.Helper()
	_, scrubber := journal.DefaultScrubber()
	return newTestDispatcherWithScrubber(t, recorder, scrubber, policy, sinks...)
}

func newTestDispatcherWithScrubber(t *testing.T, recorder Recorder, scrubber journal.Scrubber, policy Policy, sinks ...Sink) *Dispatcher {
	t.Helper()
	registry := NewRegistry()
	for _, sink := range sinks {
		if err := registry.Register(sink); err != nil {
			t.Fatal(err)
		}
	}
	dispatcher, err := NewDispatcher(registry, recorder, scrubber, policy)
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher
}

func TestSinkContractPreservesExactContent(t *testing.T) {
	tests := []struct {
		name string
		sink func(*bytes.Buffer) Sink
	}{
		{name: "recording", sink: func(*bytes.Buffer) Sink { return &RecordingSink{} }},
		{name: "terminal", sink: func(output *bytes.Buffer) Sink {
			sink, err := NewTerminalSink(output)
			if err != nil {
				t.Fatal(err)
			}
			return sink
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			sink := tt.sink(&output)
			request := validRequest(sink.Kind())
			recorder := &memoryRecorder{}
			dispatcher := newTestDispatcher(t, recorder, Policy{}, sink)
			if _, err := dispatcher.Dispatch(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if recording, ok := sink.(*RecordingSink); ok {
				got := recording.Requests()
				if len(got) != 1 || got[0].Title != request.Title || got[0].Body != request.Body || got[0].SpeechText != request.SpeechText {
					t.Fatalf("recorded request changed: %+v", got)
				}
			} else if got, want := output.String(), "exact title\nexact body\nexact speech\n"; got != want {
				t.Fatalf("terminal output = %q, want %q", got, want)
			}
		})
	}
}

func TestDispatchRetriesAndDurablySuppressesDuplicate(t *testing.T) {
	sink := &RecordingSink{Err: errors.New("temporary failure")}
	recorder := &memoryRecorder{}
	dispatcher := newTestDispatcher(t, recorder, Policy{MaxAttempts: 2}, sink)
	request := validRequest("recording")
	if result, err := dispatcher.Dispatch(context.Background(), request); err == nil || len(result.Receipts) != 2 {
		t.Fatalf("first dispatch receipts=%d err=%v", len(result.Receipts), err)
	}
	sink.Err = nil
	if _, err := dispatcher.Dispatch(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	before := len(sink.Requests())
	result, err := dispatcher.Dispatch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.Requests()) != before {
		t.Fatal("idempotent retry reached sink")
	}
	if len(result.Receipts) != 1 || result.Receipts[0].Status != apiv1.NotificationSkipped || result.Receipts[0].Attempt != 0 {
		t.Fatalf("duplicate receipt = %+v", result.Receipts)
	}
}

func TestPartialDeliveryPolicies(t *testing.T) {
	good := &RecordingSink{SinkKind: "good"}
	bad := &RecordingSink{SinkKind: "bad", Err: errors.New("no route")}
	request := validRequest("good", "bad")
	if _, err := newTestDispatcher(t, &memoryRecorder{}, Policy{Partial: RequireAny}, good, bad).Dispatch(context.Background(), request); err != nil {
		t.Fatalf("require-any returned error: %v", err)
	}
	if _, err := newTestDispatcher(t, &memoryRecorder{}, Policy{Partial: RequireAll}, good, bad).Dispatch(context.Background(), request); err == nil {
		t.Fatal("require-all accepted partial delivery")
	}
}

type uncooperativeSink struct {
	release <-chan struct{}
	calls   *atomic.Int32
}

func (uncooperativeSink) Kind() string    { return "uncooperative" }
func (uncooperativeSink) Version() string { return "v1" }
func (s uncooperativeSink) Deliver(context.Context, apiv1.NotificationRequest) (string, error) {
	if s.calls != nil {
		s.calls.Add(1)
	}
	<-s.release
	return "", nil
}

func TestDispatchTimeoutCancellationExpiryAndPayloadLimit(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int32
		recorder := &memoryRecorder{}
		dispatcher := newTestDispatcher(t, recorder, Policy{
			Timeout: 10 * time.Millisecond, MaxAttempts: 3,
		}, uncooperativeSink{release: release, calls: &calls})
		request := validRequest("uncooperative")
		started := time.Now()
		result, err := dispatcher.Dispatch(context.Background(), request)
		if err == nil || len(result.Receipts) != 1 || result.Receipts[0].Status != apiv1.NotificationFailed ||
			!strings.Contains(result.Receipts[0].Error, "deadline exceeded") {
			t.Fatalf("timeout result=%+v err=%v", result, err)
		}
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			t.Fatalf("non-cooperative sink blocked dispatcher for %s", elapsed)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("timed-out sink attempts = %d, want 1", got)
		}

		result, err = dispatcher.Dispatch(context.Background(), request)
		if err == nil || len(result.Receipts) != 1 ||
			result.Receipts[0].Status != apiv1.NotificationSkipped ||
			!strings.Contains(result.Receipts[0].Error, "remains unresolved") {
			t.Fatalf("pending retry result=%+v err=%v", result, err)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("pending retry reached sink: calls = %d, want 1", got)
		}

		close(release)
		deadline := time.Now().Add(time.Second)
		for {
			recorder.mu.Lock()
			receipts := append([]apiv1.NotificationReceipt(nil), recorder.receipts...)
			recorder.mu.Unlock()
			if len(receipts) >= 4 && receipts[len(receipts)-1].Status == apiv1.NotificationDelivered {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("late delivery receipt not recorded: %+v", receipts)
			}
			time.Sleep(time.Millisecond)
		}

		result, err = dispatcher.Dispatch(context.Background(), request)
		if err != nil || len(result.Receipts) != 1 || result.Receipts[0].Status != apiv1.NotificationSkipped {
			t.Fatalf("late delivery retry result=%+v err=%v", result, err)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("late delivery retry reached sink: calls = %d, want 1", got)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := newTestDispatcher(t, &memoryRecorder{}, Policy{}, &RecordingSink{}).Dispatch(ctx, validRequest("recording"))
		if err == nil || len(result.Receipts) != 1 || result.Receipts[0].Status != apiv1.NotificationSkipped {
			t.Fatalf("cancel result=%+v err=%v", result, err)
		}
	})
	t.Run("expiry", func(t *testing.T) {
		request := validRequest("recording")
		request.ExpiresAt = time.Now().Add(-time.Second)
		result, err := newTestDispatcher(t, &memoryRecorder{}, Policy{}, &RecordingSink{}).Dispatch(context.Background(), request)
		if err == nil || result.Receipts[0].Status != apiv1.NotificationSkipped {
			t.Fatalf("expiry result=%+v err=%v", result, err)
		}
	})
	t.Run("retry delay crosses expiry", func(t *testing.T) {
		sink := &RecordingSink{Err: errors.New("temporary failure")}
		request := validRequest("recording")
		request.ExpiresAt = time.Now().Add(20 * time.Millisecond)
		result, err := newTestDispatcher(t, &memoryRecorder{}, Policy{
			MaxAttempts: 2,
			RetryDelay:  40 * time.Millisecond,
		}, sink).Dispatch(context.Background(), request)
		if err == nil ||
			len(result.Receipts) != 2 ||
			result.Receipts[1].Status != apiv1.NotificationSkipped ||
			!strings.Contains(result.Receipts[1].Error, "expired") {
			t.Fatalf("retry expiry result=%+v err=%v", result, err)
		}
		if got := len(sink.Requests()); got != 1 {
			t.Fatalf("expired retry reached sink: calls = %d, want 1", got)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		request := validRequest("recording")
		request.Body = strings.Repeat("x", MaxBodyBytes+1)
		if _, err := newTestDispatcher(t, &memoryRecorder{}, Policy{}, &RecordingSink{}).Dispatch(context.Background(), request); err == nil {
			t.Fatal("oversized request accepted")
		}
	})
}

type referenceSink string

func (referenceSink) Kind() string    { return "reference" }
func (referenceSink) Version() string { return "v1" }
func (s referenceSink) Deliver(context.Context, apiv1.NotificationRequest) (string, error) {
	return string(s), nil
}

func TestRegistryRejectsNonCanonicalSinkIdentity(t *testing.T) {
	for _, sink := range []Sink{
		&RecordingSink{SinkKind: " recording"},
		&RecordingSink{SinkVersion: "v1 "},
	} {
		if err := NewRegistry().Register(sink); err == nil {
			t.Fatalf("registered non-canonical sink kind=%q version=%q", sink.Kind(), sink.Version())
		}
	}
}

func TestDispatchOmitsOversizedExternalReference(t *testing.T) {
	sink := referenceSink(strings.Repeat("x", MaxExternalRefRunes+1))
	recorder := &memoryRecorder{}
	result, err := newTestDispatcher(t, recorder, Policy{}, sink).Dispatch(context.Background(), validRequest("reference"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Receipts) != 1 ||
		result.Receipts[0].Status != apiv1.NotificationDelivered ||
		result.Receipts[0].ExternalReference != "" ||
		!strings.Contains(result.Receipts[0].Error, "external reference exceeds") {
		t.Fatalf("oversized external reference receipt = %+v", result.Receipts)
	}

	result, err = newTestDispatcher(t, recorder, Policy{}, sink).Dispatch(context.Background(), validRequest("reference"))
	if err != nil || len(result.Receipts) != 1 || result.Receipts[0].Status != apiv1.NotificationSkipped {
		t.Fatalf("idempotent retry result=%+v err=%v", result, err)
	}
}

func TestErrorsAreSanitized(t *testing.T) {
	token := "ghp_abcdefghijklmnopqrstuvwxyz1234567890"
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte(token))
	sink := &RecordingSink{Err: errors.New("remote\nfailed with " + token)}
	result, err := newTestDispatcherWithScrubber(t, &memoryRecorder{}, scrubber, Policy{}, sink).Dispatch(context.Background(), validRequest("recording"))
	if err == nil {
		t.Fatal("sink error reported as success")
	}
	got := result.Receipts[0].Error
	if strings.Contains(got, token) || strings.Contains(got, "\n") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("unsanitized error %q", got)
	}
}

func TestJournalRecorderPersistsRedactedRequestAndIdempotency(t *testing.T) {
	root := t.TempDir()
	runID := "0123456789abcdef0123456789abcdef"
	registry, scrubber := journal.DefaultScrubber()
	run, err := journal.Create(root, journal.RunIdentity{
		RunID: runID, Workflow: "mission-control", WorkflowVersion: 1,
		WorkflowDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Gaggle:         "goobers", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithScrubber(scrubber))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	recorder, err := NewJournalRecorder(run)
	if err != nil {
		t.Fatal(err)
	}
	sink := &RecordingSink{}
	request := validRequest("recording")
	request.Body = "token ghp_abcdefghijklmnopqrstuvwxyz1234567890"
	secret := strings.TrimPrefix(request.Body, "token ")
	registry.Register([]byte(secret))
	request.IdempotencyKey = secret
	sink.Err = errors.New("delivery rejected credential " + secret)
	dispatcher := newTestDispatcherWithScrubber(t, recorder, scrubber, Policy{}, sink)
	if _, err := dispatcher.Dispatch(context.Background(), request); err == nil {
		t.Fatal("sink error reported as success")
	}
	sink.Err = nil
	if _, err := dispatcher.Dispatch(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.Dispatch(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(sink.Requests()) != 2 {
		t.Fatalf("sink deliveries = %d, want 2", len(sink.Requests()))
	}
	raw, err := os.ReadFile(filepath.Join(root, runID, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("ghp_abcdefghijklmnopqrstuvwxyz1234567890")) ||
		bytes.Contains(raw, []byte(secret)) ||
		!bytes.Contains(raw, []byte(idempotencyDigest(secret))) ||
		!bytes.Contains(raw, []byte(journal.Redacted)) ||
		!bytes.Contains(raw, []byte(`"type":"notification.requested"`)) ||
		!bytes.Contains(raw, []byte(`"type":"notification.delivery.receipt"`)) {
		t.Fatalf("journal did not redact and persist notification records:\n%s", raw)
	}
}

func TestJournalRecorderRecoversUnresolvedAttempt(t *testing.T) {
	root := t.TempDir()
	runID := "0123456789abcdef0123456789abcdef"
	_, scrubber := journal.DefaultScrubber()
	run, err := journal.Create(root, journal.RunIdentity{
		RunID: runID, Workflow: "mission-control", WorkflowVersion: 1,
		WorkflowDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Gaggle:         "goobers", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithScrubber(scrubber))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	recorder, err := NewJournalRecorder(run)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var calls atomic.Int32
	sink := uncooperativeSink{release: release, calls: &calls}
	request := validRequest(sink.Kind())
	dispatcher := newTestDispatcherWithScrubber(t, recorder, scrubber, Policy{Timeout: 10 * time.Millisecond}, sink)
	if _, err := dispatcher.Dispatch(context.Background(), request); err == nil {
		t.Fatal("timed-out delivery reported as success")
	}

	recovered := newTestDispatcherWithScrubber(t, recorder, scrubber, Policy{Timeout: 10 * time.Millisecond}, sink)
	result, err := recovered.Dispatch(context.Background(), request)
	if err == nil || len(result.Receipts) != 1 ||
		result.Receipts[0].Status != apiv1.NotificationSkipped ||
		!strings.Contains(result.Receipts[0].Error, "remains unresolved") {
		t.Fatalf("recovered dispatch result=%+v err=%v", result, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("recovered dispatch reached sink: calls = %d, want 1", got)
	}
	close(release)
}

type claimBarrierRecorder struct {
	Recorder
	arrivals chan<- struct{}
	release  <-chan struct{}
}

func (r claimBarrierRecorder) ClaimDelivery(ctx context.Context, pending apiv1.NotificationReceipt) (apiv1.NotificationReceipt, RecordedDeliveryState, error) {
	r.arrivals <- struct{}{}
	<-r.release
	return r.Recorder.ClaimDelivery(ctx, pending)
}

func TestJournalRecorderAtomicallyClaimsConcurrentDelivery(t *testing.T) {
	root := t.TempDir()
	runID := "0123456789abcdef0123456789abcdef"
	_, scrubber := journal.DefaultScrubber()
	run, err := journal.Create(root, journal.RunIdentity{
		RunID: runID, Workflow: "mission-control", WorkflowVersion: 1,
		WorkflowDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Gaggle:         "goobers", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithScrubber(scrubber))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	firstRecorder, err := NewJournalRecorder(run)
	if err != nil {
		t.Fatal(err)
	}
	secondRecorder, err := NewJournalRecorder(run)
	if err != nil {
		t.Fatal(err)
	}
	arrivals := make(chan struct{}, 2)
	release := make(chan struct{})
	sink := &RecordingSink{}
	request := validRequest(sink.Kind())
	first := newTestDispatcherWithScrubber(t, claimBarrierRecorder{
		Recorder: firstRecorder, arrivals: arrivals, release: release,
	}, scrubber, Policy{}, sink)
	second := newTestDispatcherWithScrubber(t, claimBarrierRecorder{
		Recorder: secondRecorder, arrivals: arrivals, release: release,
	}, scrubber, Policy{}, sink)

	results := make(chan error, 2)
	go func() {
		_, dispatchErr := first.Dispatch(context.Background(), request)
		results <- dispatchErr
	}()
	go func() {
		_, dispatchErr := second.Dispatch(context.Background(), request)
		results <- dispatchErr
	}()
	<-arrivals
	<-arrivals
	close(release)
	failures := 0
	for range 2 {
		if err := <-results; err != nil {
			failures++
		}
	}
	if failures > 1 {
		t.Fatalf("concurrent dispatch failures = %d, want at most 1 suppressed claimant", failures)
	}
	if got := len(sink.Requests()); got != 1 {
		t.Fatalf("concurrent dispatch sink calls = %d, want 1", got)
	}
}
