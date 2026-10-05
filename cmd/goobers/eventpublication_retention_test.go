package main

import (
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestEventPublicationTerminalFenceCompactsWithoutReexecution(t *testing.T) {
	f := publicationHost(t, false)
	original := f.accept(t, "producer", "")
	for range 4 {
		f.drain(t)
		f.sched.Wait()
		f.wg.Wait()
	}
	page, err := f.service.queue.EventPublicationPage(t.Context(), "", 100)
	if err != nil || len(page) != 1 || page[0].SettledAt.IsZero() {
		t.Fatal(page, err)
	}
	intent := page[0]
	record, start := f.record(t, original)
	runtime, err := f.setup.EventRuntime(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Release()
	f.now = f.now.Add(triggerqueue.EventRetention)
	for range 3 {
		f.drain(t)
	}
	page, err = f.service.queue.EventPublicationPage(t.Context(), "", 100)
	if err != nil || len(page) != 0 {
		t.Fatal(page, err)
	}
	observed, err := f.service.queue.ObserveEventPublication(t.Context(), intent)
	if err != nil || observed.ReceiptID != intent.ReceiptID || observed.TerminalSequence != intent.TerminalSequence {
		t.Fatal(observed, err)
	}
	if _, _, err = f.service.queue.BeginEventPublication(t.Context(), intent, f.now); !errors.Is(err, triggerqueue.ErrEventPublicationSettled) {
		t.Fatal(err)
	}
	// Ordinary daemon recovery remains an idempotent terminal read after custody
	// compaction. It cannot re-enter the publishing task or create another intent.
	result, err := runtime.runner.Resume(t.Context(), runner.ResumeInput{RunID: record.RunID, Machine: runtime.machine, GooberDigest: runtime.gooberDigest, RepoRef: runtime.repoRef})
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatal(result, err)
	}
	page, err = f.service.queue.EventPublicationPage(t.Context(), "", 100)
	if err != nil || len(page) != 0 {
		t.Fatal(page, err)
	}
}

func TestEventPublicationFailedConsumerRetainsAncestryForTerminalResume(t *testing.T) {
	f := publicationHost(t, false)
	publisher := f.setup.EventPublisher
	originalPolicy := publisher.snapshot
	removed := originalPolicy
	removed.policies = map[string]*apiv1.GaggleEvents{}
	reloader := &configReloader{setup: f.setup}
	if err := reloader.publishEventDefinitions(removed, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	initial := f.accept(t, "failed-producer", "")
	for range 3 {
		f.drain(t)
		f.sched.Wait()
		f.wg.Wait()
	}
	record, start := f.record(t, initial)
	dir, err := f.layout.FindRunDir(record.RunID)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	phase, err := rd.Phase()
	if err != nil || phase != journal.PhaseFailed {
		t.Fatal(phase, err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var terminal uint64
	for _, e := range events {
		if e.Type == journal.EventRunFinished {
			terminal = e.Seq
		}
	}
	group, err := f.service.queue.EventGroup(t.Context(), "example", start.GroupID)
	if err != nil || !group.SettledAt.IsZero() {
		t.Fatal(group, err)
	}
	f.now = f.now.Add(8 * 24 * time.Hour)
	for range 3 {
		f.drain(t)
	}
	// Failure before the first outbox still retains consumer input receipt/group.
	if _, err = f.service.queue.Event(t.Context(), "example", initial.Producer.Binding, initial.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.service.queue.VerifiedEventStart(t.Context(), "example", start.GroupID); err != nil {
		t.Fatal(err)
	}
	if err = reloader.publishEventDefinitions(originalPolicy, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	runtime, err := f.setup.EventRuntime(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Release()
	result, err := runtime.runner.ResumeFromTerminal(t.Context(), runner.ResumeFromTerminalInput{RunID: record.RunID, Machine: runtime.machine, GooberDigest: runtime.gooberDigest, RepoRef: runtime.repoRef, Target: "local-ci", Actor: "verified-human", Action: "resume", ExpectedTerminalSeq: terminal})
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatal(result, err)
	}
	page, err := f.service.queue.EventPublicationPage(t.Context(), "", 100)
	if err != nil || len(page) != 1 || page[0].Acceptance.Producer.RootGroupID != start.GroupID {
		t.Fatal(page, err)
	}
}
