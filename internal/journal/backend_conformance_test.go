package journal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

type backendFactory func() journal.Backend

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
	} {
		t.Run(name, func(t *testing.T) {
			if err := journalBackendConformance(func() journal.Backend { return wrap(base()) }); err == nil {
				t.Fatal("broken backend passed conformance")
			}
		})
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

func conformancePersistence(backend journal.Backend) error {
	ctx := context.Background()
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
	if err := backend.AppendBatch(ctx, handle, owner, batch); err != nil {
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
	if err := backend.Retire(ctx, handle, owner); err != nil {
		return err
	}
	reopened, reopenedOwner, report, err := backend.RecoverRun(ctx, key, journal.RunWriterOptions{Scrubber: scrubber})
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
	}
	if got := eventIdentities(report.Events); !slices.Equal(got, want) {
		return fmt.Errorf("events = %+v, want %+v", got, want)
	}
	if appended, err := backend.AppendIfAbsent(ctx, reopened, reopenedOwner, once, matchOnce); err != nil || appended {
		if err != nil {
			return fmt.Errorf("reopened duplicate: %w", err)
		}
		return fmt.Errorf("reopened duplicate = %v", appended)
	}
	if existing, err := backend.ClaimNotificationDelivery(ctx, reopened, reopenedOwner, receipt); err != nil || existing == nil {
		if err != nil {
			return fmt.Errorf("reopened notification: %w", err)
		}
		return fmt.Errorf("reopened notification = %+v", existing)
	}
	if err := backend.AppendBatch(ctx, reopened, owner, []journal.Event{{Type: journal.EventRunnerAnnotation}}); err == nil {
		return errors.New("stale owner token wrote after reopen")
	}
	return backend.Retire(ctx, reopened, reopenedOwner)
}

func conformanceConcurrentBatches(backend journal.Backend) error {
	ctx := context.Background()
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
			errs <- backend.AppendBatch(ctx, handle, owner, []journal.Event{
				{Type: journal.EventRunnerAnnotation, Name: fmt.Sprintf("%02d-0", i)},
				{Type: journal.EventRunnerAnnotation, Name: fmt.Sprintf("%02d-1", i)},
			})
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
	reopened, reopenedOwner, report, err := backend.RecoverRun(ctx, key, journal.RunWriterOptions{})
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
	return backend.Retire(ctx, reopened, reopenedOwner)
}

func conformancePartialBatch(backend journal.Backend) error {
	ctx := context.Background()
	key := journal.RunKey{Gaggle: "g", RunID: "run-partial"}
	handle, owner, err := backend.CreateRun(ctx, journal.CreateRunRequest{
		Identity: journal.RunIdentity{RunID: key.RunID, Gaggle: key.Gaggle, Workflow: "implementation"},
	})
	if err != nil {
		return err
	}
	err = backend.AppendBatch(ctx, handle, owner, []journal.Event{
		{Type: journal.EventRunnerAnnotation, Name: "prefix"},
		{Type: journal.EventNotificationRequested}, // invalid: missing request
		{Type: journal.EventRunnerAnnotation, Name: "must-not-commit"},
	})
	if err == nil {
		return errors.New("invalid batch succeeded")
	}
	if err := backend.Retire(ctx, handle, owner); err != nil {
		return err
	}
	reopened, reopenedOwner, report, err := backend.RecoverRun(ctx, key, journal.RunWriterOptions{})
	if err != nil {
		return err
	}
	want := []eventIdentity{{1, journal.EventRunStarted, ""}, {2, journal.EventRunnerAnnotation, "prefix"}}
	if got := eventIdentities(report.Events); !slices.Equal(got, want) {
		return fmt.Errorf("ordered-prefix recovery = %+v, want %+v", got, want)
	}
	return backend.Retire(ctx, reopened, reopenedOwner)
}

func conformanceInstance(backend journal.Backend) error {
	ctx := context.Background()
	handle, owner, _, err := backend.OpenInstance(ctx)
	if err != nil {
		return err
	}
	if err := backend.InstanceAppend(ctx, handle, owner, journal.Event{Type: journal.EventDaemonStarted}); err != nil {
		return err
	}
	if err := backend.InstanceAppend(ctx, handle, "stale", journal.Event{Type: journal.EventRunnerAnnotation}); err == nil {
		return errors.New("stale instance owner wrote")
	}
	if err := backend.RetireInstance(ctx, handle, owner); err != nil {
		return err
	}
	reopened, reopenedOwner, report, err := backend.OpenInstance(ctx)
	if err != nil {
		return err
	}
	if report.LastSeq != 1 {
		return fmt.Errorf("instance last seq = %d", report.LastSeq)
	}
	return backend.RetireInstance(ctx, reopened, reopenedOwner)
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

type dropFirstBackend struct{ journal.Backend }

func (b *dropFirstBackend) AppendBatch(ctx context.Context, handle journal.RunHandle, owner journal.OwnerToken, events []journal.Event) error {
	if len(events) > 0 {
		events = events[1:]
	}
	return b.Backend.AppendBatch(ctx, handle, owner, events)
}

type reverseBatchBackend struct{ journal.Backend }

func (b *reverseBatchBackend) AppendBatch(ctx context.Context, handle journal.RunHandle, owner journal.OwnerToken, events []journal.Event) error {
	reversed := append([]journal.Event(nil), events...)
	slices.Reverse(reversed)
	return b.Backend.AppendBatch(ctx, handle, owner, reversed)
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
