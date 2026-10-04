package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func publicationHost(t *testing.T, matched bool) *eventHostFixture {
	t.Helper()
	return eventHostConfigured(t, func(f *eventHostFixture, source string) string {
		consumer := strings.ReplaceAll(source, "default-implement", "event-consumer")
		writeFileContent(t, filepath.Join(filepath.Dir(f.source), "event-consumer.yaml"), consumer)
		path := filepath.Join(f.layout.ConfigDir(), "gaggles/example/gaggle.yaml")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		typ := "produced"
		if !matched {
			typ = "different"
		}
		policy := "  events:\n    publishers:\n      - workflow: default-implement\n        allowedTypes: [produced]\n    subscriptions:\n      - name: consumer\n        workflow: event-consumer\n        filter:\n          all: [{attribute: type, equals: " + typ + "}]\n"
		writeFileContent(t, path, strings.Replace(string(raw), "spec:\n", "spec:\n"+policy, 1))
		return strings.Replace(source, "      run:\n", "      capabilities: [event:publish]\n      inputs: {kind: publish-event, type: produced, occurrenceKey: completed, data: '{\"answer\":42}' }\n      run:\n", 1)
	})
}

func TestEventPublicationRunnerOutboxReceiptConsumer(t *testing.T) {
	for _, matched := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-match", true: "consumer"}[matched], func(t *testing.T) {
			f := publicationHost(t, matched)
			var calls atomic.Int32
			f.setup.EventPublisher.service.Accept = func(ctx context.Context, req triggerqueue.EventAcceptance, now time.Time) (triggerqueue.EventReceipt, bool, error) {
				receipt, duplicate, err := f.service.queue.AcceptEvent(ctx, req, now)
				if err == nil && calls.Add(1) == 1 {
					return triggerqueue.EventReceipt{}, false, errors.New("simulated lost accepted reply")
				}
				return receipt, duplicate, err
			}
			original := f.accept(t, "start-publisher", "")
			// Pending edits cannot change the applied publication catalog or pins.
			writeFileContent(t, f.source, "invalid pending YAML: [")
			for range 5 {
				f.drain(t)
				f.sched.Wait()
				f.wg.Wait()
			}
			outbox, err := f.service.queue.EventPublicationPage(t.Context(), "", 100)
			if err != nil || len(outbox) != 1 || outbox[0].ReceiptID == "" {
				record, _ := f.record(t, original)
				dir, _ := f.layout.FindRunDir(record.RunID)
				rd, _ := journal.OpenReadOnly(dir)
				if rd != nil {
					events, _ := rd.Events()
					for _, e := range events {
						if e.Error != nil {
							t.Logf("event=%+v error=%+v", e, e.Error)
						}
					}
				}
				t.Fatalf("outbox=%+v err=%v", outbox, err)
			}
			if calls.Load() != 2 {
				t.Fatalf("publication attempts=%d", calls.Load())
			}
			published, err := f.service.queue.Event(t.Context(), "example", outbox[0].Acceptance.Producer.Binding, outbox[0].ReceiptID)
			if err != nil {
				t.Fatal(err)
			}
			if published.Producer.RootGroupID == "" || published.Producer.RootSetDigest == "" {
				t.Fatal("consumer publication reset causal roots")
			}
			producer, _ := f.record(t, original)
			dir, err := f.layout.FindRunDir(producer.RunID)
			if err != nil {
				t.Fatal(err)
			}
			rd, err := journal.OpenReadOnly(dir)
			if err != nil {
				t.Fatal(err)
			}
			phase, err := rd.Phase()
			if err != nil || phase != journal.PhaseCompleted {
				t.Fatalf("producer phase=%v err=%v", phase, err)
			}
			events, err := rd.Events()
			if err != nil {
				t.Fatal(err)
			}
			var occurrence string
			var starts int
			for _, e := range events {
				if e.Type != journal.EventStageStarted {
					continue
				}
				o, err := journal.PublicationOccurrence(e)
				if err != nil {
					t.Fatal(err)
				}
				if occurrence != "" && o != occurrence {
					t.Fatal("retry changed occurrence")
				}
				occurrence = o
				starts++
			}
			if starts != 2 {
				t.Fatalf("producer starts=%d", starts)
			}
			if !matched {
				if published.State != triggerqueue.EventUnmatched {
					t.Fatal(published.State)
				}
				return
			}
			record, _ := f.record(t, published)
			consumerDir, err := f.layout.FindRunDir(record.RunID)
			if err != nil {
				t.Fatal(err)
			}
			consumerReader, err := journal.OpenReadOnly(consumerDir)
			if err != nil {
				t.Fatal(err)
			}
			consumerPhase, err := consumerReader.Phase()
			if err != nil || consumerPhase != journal.PhaseCompleted {
				t.Fatalf("consumer phase=%v err=%v", consumerPhase, err)
			}
		})
	}
}

func TestEventPublicationCurrentPermissionRevocation(t *testing.T) {
	f := publicationHost(t, true)
	publisher := f.setup.EventPublisher
	next := publisher.snapshot
	next.policies = map[string]*apiv1.GaggleEvents{}
	reloader := &configReloader{setup: f.setup}
	failed := errors.New("publication failed")
	if err := reloader.publishEventDefinitions(next, func() error { return failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	if !eventing.AllowsPublication(publisher.snapshot.policies["example"], "default-implement", "produced") {
		t.Fatal("failed reload revoked applied policy")
	}
	if err := reloader.publishEventDefinitions(next, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	f.accept(t, "start-revoked", "")
	for range 3 {
		f.drain(t)
		f.sched.Wait()
		f.wg.Wait()
	}
	outbox, err := f.service.queue.EventPublicationPage(t.Context(), "", 100)
	if err != nil || len(outbox) != 0 {
		t.Fatalf("revoked publisher wrote outbox: %+v %v", outbox, err)
	}
}
