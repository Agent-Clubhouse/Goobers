package journal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

type backendStore struct{ Open func() journal.Backend }
type backendFactory func() backendStore

func TestJournalBackendConformanceRejectsBrokenBackends(t *testing.T) {
	base := fileBackendFactory(t)
	for name, wrap := range map[string]func(journal.Backend) journal.Backend{
		"drops-first-batch-event": func(next journal.Backend) journal.Backend {
			return &dropFirstBackend{Backend: next}
		},
		"reverses-batch": func(next journal.Backend) journal.Backend {
			return &reverseBatchBackend{Backend: next}
		},
		"volatile-append-if-absent": func(next journal.Backend) journal.Backend {
			return &volatileAbsentBackend{Backend: next, seen: map[string]bool{}}
		},
		"span-unscrubbed": func(next journal.Backend) journal.Backend {
			return &unscrubbedSpanBackend{Backend: next}
		},
		"span-not-persisted": func(next journal.Backend) journal.Backend {
			return &unpersistedSpanBackend{Backend: next}
		},
		"status-always-zero": func(next journal.Backend) journal.Backend {
			return &zeroStatusBackend{Backend: next}
		},
		"blob-ignores-write-token": func(next journal.Backend) journal.Backend {
			return newIgnoreWriteTokenBackend(next, "blob")
		},
		"append-if-absent-ignores-write-token": func(next journal.Backend) journal.Backend {
			return newIgnoreWriteTokenBackend(next, "append-if-absent")
		},
		"claim-ignores-write-token": func(next journal.Backend) journal.Backend {
			return newIgnoreWriteTokenBackend(next, "claim")
		},
		"checkpoint-ignores-write-token": func(next journal.Backend) journal.Backend {
			return newIgnoreWriteTokenBackend(next, "checkpoint")
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := journalBackendConformance(func() backendStore {
				store := base()
				return backendStore{Open: func() journal.Backend { return wrap(store.Open()) }}
			}); err == nil {
				t.Fatal("broken backend passed conformance")
			}
		})
	}
}

func TestJournalBackendConformanceAcceptsAllOrNothingBatch(t *testing.T) {
	base := fileBackendFactory(t)
	if err := journalBackendConformance(func() backendStore {
		store := base()
		return backendStore{Open: func() journal.Backend {
			return &allOrNothingBackend{Backend: store.Open()}
		}}
	}); err != nil {
		t.Fatal(err)
	}
}

func journalBackendConformance(factory backendFactory) error {
	if err := conformancePersistence(factory()); err != nil {
		return fmt.Errorf("persistence: %w", err)
	}
	if err := conformanceConcurrentBatches(factory()); err != nil {
		return fmt.Errorf("concurrency: %w", err)
	}
	if err := conformancePartialBatch(factory()); err != nil {
		return fmt.Errorf("partial batch: %w", err)
	}
	if err := conformanceInstance(factory()); err != nil {
		return fmt.Errorf("instance: %w", err)
	}
	return nil
}

func conformancePersistence(store backendStore) error {
	ctx := context.Background()
	backend := store.Open()
	key := journal.RunKey{Gaggle: "g", RunID: "run-persist"}
	scrubber := journal.NewRegistryScrubber()
	const secret = "backend-secret"
	scrubber.Register([]byte(secret))
	handle, owner, err := backend.CreateRun(ctx, journal.CreateRunRequest{
		Identity: journal.RunIdentity{RunID: key.RunID, Gaggle: key.Gaggle, Workflow: "implementation"},
		Inputs:   map[string][]byte{"input": []byte("pinned")},
		Options:  journal.RunWriterOptions{Scrubber: scrubber},
	})
	if err != nil {
		return err
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, _, err := store.Open().RecoverRun(cancelled, key, journal.RunWriterOptions{}); !errors.Is(err, journal.ErrRunOwned) || !errors.Is(err, context.Canceled) {
		return fmt.Errorf("second backend contention = %w", err)
	}
	if _, _, _, err := backend.RecoverRun(ctx, key, journal.RunWriterOptions{}); !errors.Is(err, journal.ErrRunOwned) {
		if err == nil {
			return errors.New("second writer succeeded")
		}
		return fmt.Errorf("second writer, want ErrRunOwned: %w", err)
	}
	batch := []journal.Event{
		{Type: journal.EventRunnerAnnotation, Name: "batch-1"},
		{Type: journal.EventRunnerAnnotation, Name: "batch-2"},
	}
	if committed, err := backend.AppendBatch(ctx, handle, owner, batch); err != nil || committed != len(batch) {
		if err != nil {
			return err
		}
		return fmt.Errorf("batch committed = %d, want %d", committed, len(batch))
	}
	status, err := backend.RunStatus(ctx, handle)
	if err != nil {
		return fmt.Errorf("status after batch: %w", err)
	}
	if status.Seq != 3 || status.Phase != journal.PhaseRunning || status.Closed {
		return fmt.Errorf("status after batch = %+v", status)
	}
	if err := assertRunWriteFencing(ctx, backend, handle); err != nil {
		return err
	}
	once := journal.Event{Type: journal.EventRunnerAnnotation, Name: "once"}
	matchOnce := func(event journal.Event) bool { return event.Type == once.Type && event.Name == once.Name }
	if appended, err := backend.AppendIfAbsent(ctx, handle, owner, once, matchOnce); err != nil || !appended {
		if err != nil {
			return fmt.Errorf("append if absent: %w", err)
		}
		return fmt.Errorf("append if absent = %v", appended)
	}
	receipt := apiv1.NotificationReceipt{
		Schema: apiv1.NotificationReceiptSchema, NotificationID: "notice",
		IdempotencyKey: "notice", IdempotencyDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Sink: apiv1.NotificationSinkRef{Kind: "terminal", Version: "v1"}, Attempt: 1, Status: apiv1.NotificationPending,
	}
	if existing, err := backend.ClaimNotificationDelivery(ctx, handle, owner, receipt); err != nil || existing != nil {
		if err != nil {
			return fmt.Errorf("notification claim: %w", err)
		}
		return fmt.Errorf("notification claim = %+v", existing)
	}
	ref, err := backend.RecordBlob(ctx, handle, owner, journal.BlobRecord{
		Event: journal.Event{Type: journal.EventArtifactRecorded, Name: "secret.txt", Integrity: apiv1.IntegrityDerived},
		Data:  []byte("value=" + secret),
	})
	if err != nil {
		return err
	}
	wantBlob := []byte("value=[REDACTED]")
	if ref.Digest != journal.Digest(wantBlob) {
		return fmt.Errorf("blob digest = %s, want scrubbed digest", ref.Digest)
	}
	if data, err := backend.ReadBlob(ctx, handle, ref); err != nil || !bytes.Equal(data, wantBlob) {
		if err != nil {
			return fmt.Errorf("blob read: %w", err)
		}
		return fmt.Errorf("blob read = %q", data)
	}
	span, err := backend.RecordSpan(ctx, handle, owner, journal.SpanRecord{
		Stage: "implementation", Name: "trace", DataSchema: "text/plain", Data: []byte("span=" + secret),
	})
	if err != nil {
		return err
	}
	wantSpan := []byte("span=[REDACTED]")
	if span.Digest != journal.Digest(wantSpan) {
		return fmt.Errorf("span digest = %s, want scrubbed digest", span.Digest)
	}
	data, err := backend.ReadBlob(ctx, handle, span)
	if err != nil {
		return fmt.Errorf("span read: %w", err)
	}
	if !bytes.Equal(data, wantSpan) {
		return fmt.Errorf("span read = %q", data)
	}
	if err := backend.Checkpoint(ctx, handle, owner); err != nil {
		return err
	}
	childSeq, origin, err := backend.AppendChildStageStarted(ctx, handle, owner, journal.ChildStageStartRecord{
		Event: journal.Event{Type: journal.EventStageStarted, Stage: "child", Attempt: 1},
	})
	if err != nil {
		return fmt.Errorf("child start: %w", err)
	}
	if origin == nil {
		return fmt.Errorf("child start = seq:%d origin:nil", childSeq)
	}
	status, err = backend.RunStatus(ctx, handle)
	if err != nil {
		return fmt.Errorf("child status: %w", err)
	}
	if status.Seq != childSeq {
		return fmt.Errorf("child status = %+v", status)
	}
	request := operatorMessageRequest()
	firstMessage, accepted, err := backend.AcceptOperatorMessage(ctx, handle, owner, request)
	if err != nil {
		return fmt.Errorf("operator accept: %w", err)
	}
	if !accepted {
		return fmt.Errorf("operator accept = %+v, false", firstMessage)
	}
	if err := assertOperatorWriteFencing(ctx, backend, handle, request); err != nil {
		return err
	}
	replayedMessage, accepted, err := backend.AcceptOperatorMessage(ctx, handle, owner, request)
	if err != nil {
		return fmt.Errorf("operator replay: %w", err)
	}
	if accepted || replayedMessage.Request.RequestID != firstMessage.Request.RequestID {
		return fmt.Errorf("operator replay = %+v, accepted:%v", replayedMessage, accepted)
	}
	transcript, err := backend.BeginTranscript(ctx, handle, owner, 0, "implementation", "transcript")
	if err != nil {
		return err
	}
	finished := journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}
	committed, err := backend.AppendBatch(ctx, handle, owner, []journal.Event{finished})
	if err != nil {
		return fmt.Errorf("terminal append: %w", err)
	}
	if committed != 1 {
		return fmt.Errorf("terminal append = %d", committed)
	}
	status, err = backend.RunStatus(ctx, handle)
	if err != nil {
		return fmt.Errorf("terminal status: %w", err)
	}
	if status.Phase != journal.PhaseCompleted || status.Closed {
		return fmt.Errorf("terminal status = %+v", status)
	}
	if err := backend.Retire(ctx, handle, owner); err != nil {
		return err
	}
	if err := transcript.Append(journal.TranscriptCheckpoint{Stream: "process-output/1", Data: []byte("late"), Reason: "checkpoint"}); !errors.Is(err, journal.ErrClosed) {
		if err == nil {
			return errors.New("transcript append after retire succeeded")
		}
		return fmt.Errorf("transcript append after retire: %w", err)
	}
	if _, err := backend.RunStatus(ctx, handle); !errors.Is(err, journal.ErrClosed) {
		if err == nil {
			return errors.New("retired status succeeded")
		}
		return fmt.Errorf("retired status: %w", err)
	}
	reopenBackend := store.Open()
	reopened, reopenedOwner, report, err := reopenBackend.RecoverRun(ctx, key, journal.RunWriterOptions{Scrubber: scrubber})
	if err != nil {
		return err
	}
	want := []eventIdentity{
		{1, journal.EventRunStarted, ""},
		{2, journal.EventRunnerAnnotation, "batch-1"},
		{3, journal.EventRunnerAnnotation, "batch-2"},
		{4, journal.EventRunnerAnnotation, "once"},
		{5, journal.EventNotificationReceipt, ""},
		{6, journal.EventArtifactRecorded, "secret.txt"},
		{7, journal.EventSpanRecorded, "trace"},
		{8, journal.EventStageStarted, ""},
		{9, journal.EventOperatorMessageRequested, ""},
		{10, journal.EventRunFinished, ""},
	}
	if got := eventIdentities(report.Events); !slices.Equal(got, want) {
		return fmt.Errorf("events = %+v, want %+v", got, want)
	}
	if appended, err := reopenBackend.AppendIfAbsent(ctx, reopened, reopenedOwner, once, matchOnce); err != nil || appended {
		if err != nil {
			return fmt.Errorf("reopened duplicate: %w", err)
		}
		return fmt.Errorf("reopened duplicate = %v", appended)
	}
	if existing, err := reopenBackend.ClaimNotificationDelivery(ctx, reopened, reopenedOwner, receipt); err != nil || existing == nil {
		if err != nil {
			return fmt.Errorf("reopened notification: %w", err)
		}
		return fmt.Errorf("reopened notification = %+v", existing)
	}
	if _, err := reopenBackend.AppendBatch(ctx, reopened, owner, []journal.Event{{Type: journal.EventRunnerAnnotation}}); err == nil {
		return errors.New("stale owner token wrote after reopen")
	}
	return reopenBackend.Retire(ctx, reopened, reopenedOwner)
}

func conformanceConcurrentBatches(store backendStore) error {
	ctx := context.Background()
	backend := store.Open()
	key := journal.RunKey{Gaggle: "g", RunID: "run-concurrent"}
	handle, owner, err := backend.CreateRun(ctx, journal.CreateRunRequest{
		Identity: journal.RunIdentity{RunID: key.RunID, Gaggle: key.Gaggle, Workflow: "implementation"},
	})
	if err != nil {
		return err
	}
	const writers = 12
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		i := i
		wg.Go(func() {
			committed, err := backend.AppendBatch(ctx, handle, owner, []journal.Event{
				{Type: journal.EventRunnerAnnotation, Name: fmt.Sprintf("%02d-0", i)},
				{Type: journal.EventRunnerAnnotation, Name: fmt.Sprintf("%02d-1", i)},
			})
			if err == nil && committed != 2 {
				err = fmt.Errorf("committed = %d, want 2", committed)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	if err := backend.Retire(ctx, handle, owner); err != nil {
		return err
	}
	reopenBackend := store.Open()
	reopened, reopenedOwner, report, err := reopenBackend.RecoverRun(ctx, key, journal.RunWriterOptions{})
	if err != nil {
		return err
	}
	if len(report.Events) != 1+writers*2 {
		return fmt.Errorf("event count = %d", len(report.Events))
	}
	for i, event := range report.Events {
		if event.Seq != uint64(i+1) {
			return fmt.Errorf("sequence[%d] = %d", i, event.Seq)
		}
	}
	positions := map[string]int{}
	for i, event := range report.Events[1:] {
		positions[event.Name] = i
	}
	for i := range writers {
		if positions[fmt.Sprintf("%02d-1", i)] != positions[fmt.Sprintf("%02d-0", i)]+1 {
			return fmt.Errorf("batch %d was reordered or interleaved", i)
		}
	}
	return reopenBackend.Retire(ctx, reopened, reopenedOwner)
}

func conformancePartialBatch(store backendStore) error {
	ctx := context.Background()
	backend := store.Open()
	key := journal.RunKey{Gaggle: "g", RunID: "run-partial"}
	handle, owner, err := backend.CreateRun(ctx, journal.CreateRunRequest{
		Identity: journal.RunIdentity{RunID: key.RunID, Gaggle: key.Gaggle, Workflow: "implementation"},
	})
	if err != nil {
		return err
	}
	committed, err := backend.AppendBatch(ctx, handle, owner, []journal.Event{
		{Type: journal.EventRunnerAnnotation, Name: "prefix-0"},
		{Type: journal.EventRunnerAnnotation, Name: "prefix-1"},
		{Type: journal.EventRunnerAnnotation, Name: "prefix-2"},
	})
	if committed < 0 || committed > 3 || (err == nil && committed != 3) {
		if err != nil {
			return fmt.Errorf("batch committed %d events: %w", committed, err)
		}
		return fmt.Errorf("batch committed %d events without error", committed)
	}
	if err := backend.Retire(ctx, handle, owner); err != nil {
		return err
	}
	reopenBackend := store.Open()
	reopened, reopenedOwner, report, err := reopenBackend.RecoverRun(ctx, key, journal.RunWriterOptions{})
	if err != nil {
		return err
	}
	want := []eventIdentity{{1, journal.EventRunStarted, ""}}
	for i := range committed {
		want = append(want, eventIdentity{uint64(i + 2), journal.EventRunnerAnnotation, fmt.Sprintf("prefix-%d", i)})
	}
	if got := eventIdentities(report.Events); !slices.Equal(got, want) {
		return fmt.Errorf("ordered-prefix recovery = %+v, want %+v", got, want)
	}
	if err := reopenBackend.Retire(ctx, reopened, reopenedOwner); err != nil {
		return err
	}
	validationKey := journal.RunKey{Gaggle: "g", RunID: "run-validation"}
	validation, validationOwner, err := backend.CreateRun(ctx, journal.CreateRunRequest{
		Identity: journal.RunIdentity{RunID: validationKey.RunID, Gaggle: validationKey.Gaggle, Workflow: "implementation"},
	})
	if err != nil {
		return err
	}
	committed, err = backend.AppendBatch(ctx, validation, validationOwner, []journal.Event{
		{Type: journal.EventRunnerAnnotation, Name: "valid"},
		{Type: journal.EventNotificationRequested},
	})
	if err == nil || committed != 0 {
		if err == nil {
			return fmt.Errorf("validation batch committed %d events without error", committed)
		}
		return fmt.Errorf("validation failure committed %d events: %w", committed, err)
	}
	if err := backend.Retire(ctx, validation, validationOwner); err != nil {
		return err
	}
	validationBackend := store.Open()
	recovered, recoveredOwner, validationReport, err := validationBackend.RecoverRun(ctx, validationKey, journal.RunWriterOptions{})
	if err != nil {
		return err
	}
	if got := eventIdentities(validationReport.Events); !slices.Equal(got, []eventIdentity{{1, journal.EventRunStarted, ""}}) {
		return fmt.Errorf("validation failure events = %+v", got)
	}
	return validationBackend.Retire(ctx, recovered, recoveredOwner)
}

func conformanceInstance(store backendStore) error {
	ctx := context.Background()
	backend := store.Open()
	handle, owner, _, err := backend.OpenInstance(ctx)
	if err != nil {
		return err
	}
	if err := backend.InstanceAppend(ctx, handle, owner, journal.Event{Type: journal.EventDaemonStarted}); err != nil {
		return err
	}
	for _, fencedOwner := range []journal.OwnerToken{"", "stale"} {
		if err := backend.InstanceAppend(ctx, handle, fencedOwner, journal.Event{Type: journal.EventRunnerAnnotation}); err == nil {
			return fmt.Errorf("instance append accepted owner token %q", fencedOwner)
		}
		if err := backend.RetireInstance(ctx, handle, fencedOwner); err == nil {
			return fmt.Errorf("instance retire accepted owner token %q", fencedOwner)
		}
	}
	if err := backend.RetireInstance(ctx, handle, owner); err != nil {
		return err
	}
	reopenBackend := store.Open()
	reopened, reopenedOwner, report, err := reopenBackend.OpenInstance(ctx)
	if err != nil {
		return err
	}
	if report.LastSeq != 1 {
		return fmt.Errorf("instance last seq = %d", report.LastSeq)
	}
	return reopenBackend.RetireInstance(ctx, reopened, reopenedOwner)
}

type eventIdentity struct {
	Seq  uint64
	Type journal.EventType
	Name string
}

func eventIdentities(events []journal.Event) []eventIdentity {
	result := make([]eventIdentity, 0, len(events))
	for _, event := range events {
		result = append(result, eventIdentity{event.Seq, event.Type, event.Name})
	}
	return result
}

func assertRunWriteFencing(ctx context.Context, backend journal.Backend, handle journal.RunHandle) error {
	notification := apiv1.NotificationReceipt{
		Schema: apiv1.NotificationReceiptSchema, NotificationID: "fenced",
		IdempotencyKey: "fenced", IdempotencyDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Sink: apiv1.NotificationSinkRef{Kind: "terminal", Version: "v1"}, Attempt: 1, Status: apiv1.NotificationPending,
	}
	request := operatorMessageRequest()
	for _, owner := range []journal.OwnerToken{"", "stale"} {
		checks := map[string]func() error{
			"append batch": func() error {
				_, err := backend.AppendBatch(ctx, handle, owner, []journal.Event{{Type: journal.EventRunnerAnnotation}})
				return err
			},
			"append if absent": func() error {
				_, err := backend.AppendIfAbsent(ctx, handle, owner, journal.Event{Type: journal.EventRunnerAnnotation}, func(journal.Event) bool { return false })
				return err
			},
			"notification claim": func() error {
				_, err := backend.ClaimNotificationDelivery(ctx, handle, owner, notification)
				return err
			},
			"checkpoint": func() error { return backend.Checkpoint(ctx, handle, owner) },
			"record blob": func() error {
				_, err := backend.RecordBlob(ctx, handle, owner, journal.BlobRecord{
					Event: journal.Event{Type: journal.EventArtifactRecorded, Name: "fenced", Integrity: apiv1.IntegrityDerived},
					Data:  []byte("fenced"),
				})
				return err
			},
			"record span": func() error {
				_, err := backend.RecordSpan(ctx, handle, owner, journal.SpanRecord{Stage: "stage", Name: "fenced", Data: []byte("fenced")})
				return err
			},
			"child start": func() error {
				_, _, err := backend.AppendChildStageStarted(ctx, handle, owner, journal.ChildStageStartRecord{
					Event: journal.Event{Type: journal.EventStageStarted, Stage: "child", Attempt: 1},
				})
				return err
			},
			"begin transcript": func() error {
				_, err := backend.BeginTranscript(ctx, handle, owner, 0, "stage", "fenced")
				return err
			},
			"repair": func() error { return backend.RepairAppendBoundary(ctx, handle, owner) },
			"operator accept": func() error {
				_, _, err := backend.AcceptOperatorMessage(ctx, handle, owner, request)
				return err
			},
			"operator reject": func() error {
				_, _, err := backend.RejectOperatorMessage(ctx, handle, owner, request, "denied", "denied")
				return err
			},
			"retire": func() error { return backend.Retire(ctx, handle, owner) },
		}
		for name, check := range checks {
			if err := check(); err == nil {
				return fmt.Errorf("%s accepted owner token %q", name, owner)
			}
		}
	}
	return nil
}

func assertOperatorWriteFencing(ctx context.Context, backend journal.Backend, handle journal.RunHandle, request apiv1.OperatorMessageRequest) error {
	ack := apiv1.OperatorMessageAcknowledgement{
		Schema: apiv1.OperatorMessageAcknowledgementSchema, RequestID: request.RequestID,
		IdempotencyKey: request.IdempotencyKey, PrincipalRef: "operator:reviewer", AcknowledgedAt: time.Now().UTC(),
	}
	outcome := apiv1.OperatorMessageOutcome{
		Schema: apiv1.OperatorMessageOutcomeSchema, RequestID: request.RequestID,
		IdempotencyKey: request.IdempotencyKey, CompletedAt: time.Now().UTC(), Status: apiv1.OperatorMessageDelivered,
	}
	for _, owner := range []journal.OwnerToken{"", "stale"} {
		if _, err := backend.AcknowledgeOperatorMessage(ctx, handle, owner, ack); err == nil {
			return fmt.Errorf("operator acknowledge accepted owner token %q", owner)
		}
		if _, err := backend.CompleteOperatorMessage(ctx, handle, owner, outcome); err == nil {
			return fmt.Errorf("operator complete accepted owner token %q", owner)
		}
	}
	return nil
}

func operatorMessageRequest() apiv1.OperatorMessageRequest {
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	return apiv1.OperatorMessageRequest{
		Schema: apiv1.OperatorMessageRequestSchema, RequestID: "request-1", IdempotencyKey: "operator-1",
		TargetAddress: "terminal:operator", PrincipalRef: "user:requester",
		RequestedAt: now, ExpiresAt: &expires, Purpose: "approval-required",
		Content: apiv1.OperatorMessageContent{Text: "Please review."}, DeliveryMode: "terminal",
	}
}

type dropFirstBackend struct{ journal.Backend }

func (b *dropFirstBackend) AppendBatch(ctx context.Context, handle journal.RunHandle, owner journal.OwnerToken, events []journal.Event) (int, error) {
	if len(events) > 0 {
		events = events[1:]
	}
	return b.Backend.AppendBatch(ctx, handle, owner, events)
}

type reverseBatchBackend struct{ journal.Backend }

func (b *reverseBatchBackend) AppendBatch(ctx context.Context, handle journal.RunHandle, owner journal.OwnerToken, events []journal.Event) (int, error) {
	reversed := append([]journal.Event(nil), events...)
	slices.Reverse(reversed)
	return b.Backend.AppendBatch(ctx, handle, owner, reversed)
}

type allOrNothingBackend struct{ journal.Backend }

func (b *allOrNothingBackend) AppendBatch(ctx context.Context, handle journal.RunHandle, owner journal.OwnerToken, events []journal.Event) (int, error) {
	if len(events) == 3 && events[0].Name == "prefix-0" {
		return 0, errors.New("injected transaction rollback")
	}
	return b.Backend.AppendBatch(ctx, handle, owner, events)
}

type unscrubbedSpanBackend struct{ journal.Backend }

func (b *unscrubbedSpanBackend) RecordSpan(_ context.Context, _ journal.RunHandle, _ journal.OwnerToken, record journal.SpanRecord) (journal.Ref, error) {
	return journal.Ref{Digest: journal.Digest(record.Data), Size: int64(len(record.Data)), Path: "spans/lost"}, nil
}

type unpersistedSpanBackend struct{ journal.Backend }

func (b *unpersistedSpanBackend) RecordSpan(context.Context, journal.RunHandle, journal.OwnerToken, journal.SpanRecord) (journal.Ref, error) {
	data := []byte("span=[REDACTED]")
	return journal.Ref{Digest: journal.Digest(data), Size: int64(len(data)), Path: "spans/lost"}, nil
}

type zeroStatusBackend struct{ journal.Backend }

func (b *zeroStatusBackend) RunStatus(context.Context, journal.RunHandle) (journal.RunStatus, error) {
	return journal.RunStatus{}, nil
}

type ignoreWriteTokenBackend struct {
	journal.Backend
	mu     sync.Mutex
	owners map[journal.RunHandle]journal.OwnerToken
	method string
}

func newIgnoreWriteTokenBackend(next journal.Backend, method string) journal.Backend {
	return &ignoreWriteTokenBackend{Backend: next, owners: map[journal.RunHandle]journal.OwnerToken{}, method: method}
}

func (b *ignoreWriteTokenBackend) CreateRun(ctx context.Context, request journal.CreateRunRequest) (journal.RunHandle, journal.OwnerToken, error) {
	handle, owner, err := b.Backend.CreateRun(ctx, request)
	b.remember(handle, owner, err)
	return handle, owner, err
}

func (b *ignoreWriteTokenBackend) RecoverRun(ctx context.Context, key journal.RunKey, options journal.RunWriterOptions) (journal.RunHandle, journal.OwnerToken, journal.RecoverReport, error) {
	handle, owner, report, err := b.Backend.RecoverRun(ctx, key, options)
	b.remember(handle, owner, err)
	return handle, owner, report, err
}

func (b *ignoreWriteTokenBackend) AppendIfAbsent(ctx context.Context, handle journal.RunHandle, owner journal.OwnerToken, event journal.Event, match func(journal.Event) bool) (bool, error) {
	if b.method == "append-if-absent" {
		owner = b.owner(handle)
	}
	return b.Backend.AppendIfAbsent(ctx, handle, owner, event, match)
}

func (b *ignoreWriteTokenBackend) ClaimNotificationDelivery(ctx context.Context, handle journal.RunHandle, owner journal.OwnerToken, receipt apiv1.NotificationReceipt) (*apiv1.NotificationReceipt, error) {
	if b.method == "claim" {
		owner = b.owner(handle)
	}
	return b.Backend.ClaimNotificationDelivery(ctx, handle, owner, receipt)
}

func (b *ignoreWriteTokenBackend) Checkpoint(ctx context.Context, handle journal.RunHandle, owner journal.OwnerToken) error {
	if b.method == "checkpoint" {
		owner = b.owner(handle)
	}
	return b.Backend.Checkpoint(ctx, handle, owner)
}

func (b *ignoreWriteTokenBackend) RecordBlob(ctx context.Context, handle journal.RunHandle, owner journal.OwnerToken, record journal.BlobRecord) (journal.Ref, error) {
	if b.method == "blob" {
		owner = b.owner(handle)
	}
	return b.Backend.RecordBlob(ctx, handle, owner, record)
}

func (b *ignoreWriteTokenBackend) remember(handle journal.RunHandle, owner journal.OwnerToken, err error) {
	if err == nil {
		b.mu.Lock()
		b.owners[handle] = owner
		b.mu.Unlock()
	}
}

func (b *ignoreWriteTokenBackend) owner(handle journal.RunHandle) journal.OwnerToken {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.owners[handle]
}

type volatileAbsentBackend struct {
	journal.Backend
	mu   sync.Mutex
	seen map[string]bool
}

func (b *volatileAbsentBackend) AppendIfAbsent(_ context.Context, _ journal.RunHandle, _ journal.OwnerToken, event journal.Event, _ func(journal.Event) bool) (bool, error) {
	key := string(event.Type) + "\x00" + event.Name
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen[key] {
		return false, nil
	}
	b.seen[key] = true
	return true, nil
}
