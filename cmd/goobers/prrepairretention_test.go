package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

func repairRetentionInput(t *testing.T, q *triggerqueue.Store, now time.Time) (triggerqueue.PRRepairCommandInput, sessioning.ExecutionInputs) {
	t.Helper()
	actor := sessioning.Actor{Issuer: "https://issuer", Subject: "alice"}
	command := triggerqueue.SessionCommand{Gaggle: "gaggle", Actor: actor, RequestID: "create", RequestDigest: sessioning.Digest([]byte("create"))}
	profile := sessioning.Profile{Goober: "planner", ConfigGeneration: sessioning.Digest([]byte("config")), GooberDigest: sessioning.Digest([]byte("goober"))}
	conversation, err := q.CreateSession(t.Context(), command, "Repair", profile, now)
	if err != nil {
		t.Fatal(err)
	}
	selection := sessioning.PRRepairTarget{SourceBindingID: "code", Repository: sessioning.RepairRepository{Provider: "github", Owner: "org", Name: "repo"}, RepositorySourceID: "100", ID: "12", SourceID: "900", ExpectedHeadSHA: strings.Repeat("a", 40)}
	command.RequestID, command.RequestDigest = "human", sessioning.Digest([]byte("human"))
	accepted, err := q.SubmitSessionInput(t.Context(), command, conversation.Session.ID, sessioning.MessageRequest{Text: "Repair selected PR", RepairTarget: &selection}, []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := q.SessionInputs(t.Context(), accepted.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := q.BeginSessionTurn(t.Context(), accepted.AcceptanceID, now)
	if err != nil {
		t.Fatal(err)
	}
	run := strings.TrimPrefix(accepted.AcceptanceID, "trigger-")
	raw, err := inputs.Validate(run, "gaggle")
	if err != nil {
		t.Fatal(err)
	}
	content := "fixed\n"
	input := triggerqueue.PRRepairCommandInput{Scope: triggerqueue.WorkbenchCommandScope{Gaggle: "gaggle", SourceBindingID: "code", Actor: actor}, RequestID: "repair", Selection: selection, Request: sessioning.PRRepairRequest{RequestID: "model", ExpectedHeadSHA: selection.ExpectedHeadSHA, Rationale: "Fix reported issue", Changes: []sessioning.PRRepairChange{{Path: "file.txt", Content: &content}}}, Origin: sessioning.PRRepairOrigin{RunID: run, SessionID: turn.Session.ID, TurnID: turn.ID, MessageID: turn.Message.ID, MessageDigest: inputs.Start.MessageDigest, ConfigGeneration: profile.ConfigGeneration, GooberDigest: profile.GooberDigest, EnvelopeDigest: sessioning.Digest(turn.Record.Payload), InputDigest: sessioning.Digest(raw)}, Target: &providers.RepairPullRequest{Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "org", Name: "repo"}, RepositoryID: "100", ID: "12", StableID: "900", Head: "fix", Base: "main", HeadSHA: selection.ExpectedHeadSHA, BaseSHA: strings.Repeat("b", 40), Open: true}}
	input.TargetDigest, input.OperationDigest, err = triggerqueue.PRRepairDigests(input.Scope, input.Selection, input.Request)
	if err != nil {
		t.Fatal(err)
	}
	return input, inputs
}

func TestPRRepairUnknownEffectPinsFinishedJournalAndClosedSessionConfig(t *testing.T) {
	q, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	input, inputs := repairRetentionInput(t, q, now)
	origin := input.Origin
	id := journal.RunIdentity{RunID: origin.RunID, Gaggle: "gaggle", Workflow: "interactive-session", WorkflowDigest: sessioning.Digest([]byte("workflow")), GooberDigest: origin.GooberDigest, ConfigGeneration: origin.ConfigGeneration, Trigger: journal.Trigger{Kind: journal.TriggerSignal, Ref: "session:" + origin.SessionID + ":" + origin.TurnID}, Session: &journal.SessionLineage{Gaggle: "gaggle", SessionID: origin.SessionID, TurnID: origin.TurnID, MessageID: origin.MessageID, AcceptanceID: inputs.AcceptanceID, EnvelopeDigest: origin.EnvelopeDigest, InputDigest: origin.InputDigest}}
	raw, _ := inputs.Validate(id.RunID, id.Gaggle)
	runs := filepath.Join(t.TempDir(), "runs")
	writer, err := journal.Create(runs, id, map[string][]byte{sessioning.ContextInputName: raw})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if err = q.ObserveSessionRun(t.Context(), inputs.AcceptanceID, id.RunID, now); err != nil {
		t.Fatal(err)
	}
	record, _, err := q.AcceptPRRepairCommand(t.Context(), input, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := q.ClaimPRRepairCommand(t.Context(), input.Scope, record.ID, record.RequestDigest, now); err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	if _, err = q.CompletePRRepairCommand(t.Context(), input.Scope, record.ID, record.RequestDigest, sessioning.PRRepairReceipt{OperationDigest: input.OperationDigest, Outcome: "unknown", MutationAttempted: true}, now); err != nil {
		t.Fatal(err)
	}
	if err = writer.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = q.CompleteSessionTurn(t.Context(), inputs.AcceptanceID, triggerqueue.SessionCompletion{RunID: id.RunID, Outcome: "success", Text: "Uncertain provider response"}, now); err != nil {
		t.Fatal(err)
	}
	closeCommand := triggerqueue.SessionCommand{Gaggle: id.Gaggle, Actor: input.Scope.Actor, RequestID: "close", RequestDigest: sessioning.Digest([]byte("close"))}
	if _, err = q.CloseSession(t.Context(), closeCommand, origin.SessionID, "done", now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(365 * 24 * time.Hour)
	if _, err = q.PruneWorkbenchCommands(t.Context(), later, 100); err != nil {
		t.Fatal(err)
	}
	if _, err = q.PruneSessions(t.Context(), later, 100); err != nil {
		t.Fatal(err)
	}
	candidate := retention.Result{RunID: id.RunID, RunDir: filepath.Join(runs, id.RunID)}
	if err = acknowledgeTriggerBeforePrune(t.Context(), q, candidate, later); !errors.Is(err, retention.ErrCustodyHeld) {
		t.Fatal("production journal prune lost uncertain repair", err)
	}
	pins := map[string]bool{}
	if err = retainSessionGenerationPins(t.Context(), q, pins); err != nil || !pins[id.ConfigGeneration] {
		t.Fatal("production config prune lost repair input", pins, err)
	}
}
