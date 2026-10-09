package main

import (
	"encoding/json"
	"github.com/goobers/goobers/internal/credentials"
	"testing"

	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestChildPodLateCustodyRequiresOriginalUnjoinedContract(t *testing.T) {
	for _, mode := range []string{"terminal", "joined", "newer", "unstarted", "wrong-origin"} {
		t.Run(mode, func(t *testing.T) {
			f := actualChildLaunchFixture(t)
			id := publishInterruptedChild(t, f)
			s := &daemonCredentialService{layout: f.launcher.layout, childQueue: f.service.queue}
			digest, writer := publishChildPodContract(t, f, id, "check", 1)
			defer func() { _ = writer.Close() }()
			blobs := &childInvocationBlobs{ScopedBlobs: childpod.ScopedBlobs{Queue: f.service.queue, Identity: f.submission.Child.Identity}, recorder: writer}
			raw, err := blobs.Get(t.Context(), digest)
			if err != nil {
				t.Fatal(err)
			}
			contract, err := childpod.DecodeContract(raw, digest)
			if err != nil {
				t.Fatal(err)
			}
			retained := childpod.RetainedAttempt{Version: 1, Input: engine.ChildDispatchInput{Attempt: dispatcher.Attempt{RunID: id.RunID, Gaggle: id.Gaggle, Workflow: id.Workflow, Stage: contract.Stage, Number: contract.Attempt, PodAttempt: contract.PodAttempt, ChildExecutionDigest: digest}, Queue: "worker", Eligible: []dispatcher.RunnerSpec{{Name: "linux", OS: "linux", HostKind: instance.RunnerHostImage}}}}
			if err := blobs.keepAttempt(t.Context(), retained); err != nil {
				t.Fatal(err)
			}
			marker := journal.Event{Type: journal.EventRunnerAnnotation, Stage: "check", Attempt: 1, Runner: map[string]any{"kind": childPodWriterStarted, "contractDigest": digest, "retainedAttempt": blobs.retainedRef}}
			if mode == "wrong-origin" {
				marker.Attempt = 2
			}
			if mode != "unstarted" {
				if err := writer.Append(marker); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Append(journal.Event{Type: journal.EventStageFinished, Stage: "check", Attempt: 1, Status: "failure"}); err != nil {
				t.Fatal(err)
			}
			if mode == "joined" {
				marker.Runner["kind"] = childPodWriterJoined
				if err := writer.Append(marker); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "newer" {
				if err := writer.Append(journal.Event{Type: journal.EventStageStarted, Stage: "check", Attempt: 2}); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Append(journal.Event{Type: journal.EventRunFinished, Status: "failed"}); err != nil {
				t.Fatal(err)
			}
			reader, err := journal.OpenReadOnly(writer.Dir())
			if err != nil {
				t.Fatal(err)
			}
			pending, err := s.childPodCustodyPending(t.Context(), reader, digest)
			if mode == "terminal" {
				if err != nil || !pending {
					t.Fatal("late custody refused", pending, err)
				}
			} else if pending {
				t.Fatal("invalid physical custody authorized", mode, err)
			}
		})
	}
}

func publishChildPodContract(t *testing.T, f *actualChildFixture, id journal.RunIdentity, stage string, attempt int) (string, *journal.Run) {
	t.Helper()
	dir, err := f.launcher.layout.FindRunDir(id.RunID)
	if err != nil {
		t.Fatal(err)
	}
	writer, _, err := journal.Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.Append(journal.Event{Type: journal.EventStageStarted, Stage: stage, Attempt: attempt}); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	event, err := childPodStarted(reader, stage, attempt, false)
	if err != nil {
		t.Fatal(err)
	}
	c := childpod.Contract{Version: 1, Identity: id, Stage: stage, Attempt: attempt, PodAttempt: int(event.Seq), StartedAt: event.Time, Ceiling: credentials.NewChildCeiling(false, []string{"agent:model"}, []string{"agent:model"})}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	digest := journal.Digest(raw)
	blobs := childpod.ScopedBlobs{Queue: f.service.queue, Identity: f.submission.Child.Identity}
	if err = blobs.Put(t.Context(), digest, raw); err != nil {
		t.Fatal(err)
	}
	if err = blobs.BindContract(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	return digest, writer
}
