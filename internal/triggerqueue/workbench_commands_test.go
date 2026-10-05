package triggerqueue

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
)

func workbenchInput(key string) WorkbenchCommandInput {
	value := "Human operations"
	return WorkbenchCommandInput{Scope: WorkbenchCommandScope{Gaggle: "team", SourceBindingID: "backlog", Actor: sessioning.Actor{Issuer: "https://identity.example", Subject: "alice"}}, RequestID: key, TargetDigest: strings.Repeat("a", 64), OperationDigest: strings.Repeat("b", 64), Request: workbench.BacklogPatchRequest{ID: "12", SourceID: "9876", ExpectedRevision: "revision-1", Field: "title", Value: &value}}
}
func acceptWorkbench(t *testing.T, s *Store, key string) WorkbenchCommand {
	t.Helper()
	record, duplicate, err := s.AcceptWorkbenchCommand(t.Context(), workbenchInput(key), childTestTime)
	if err != nil || duplicate {
		t.Fatalf("accept=%+v duplicate=%v err=%v", record, duplicate, err)
	}
	return record
}
func claimWorkbench(t *testing.T, s *Store, record WorkbenchCommand) WorkbenchCommand {
	t.Helper()
	result, claimed, err := s.ClaimWorkbenchCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, childTestTime.Add(time.Second))
	if err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	return result
}
func workbenchReceipt(record WorkbenchCommand, outcome string) workbench.BacklogPatchReceipt {
	receipt := workbench.BacklogPatchReceipt{OperationDigest: record.Input.OperationDigest, Outcome: outcome, RevisionSemantics: "timestamp-preflight"}
	if outcome == "confirmed" {
		receipt.ProviderAcknowledged = true
		receipt.ObservedMatches = true
		receipt.Observed = &workbench.BacklogItem{Ref: workbench.NodeRef{GaggleID: record.Input.Scope.Gaggle, SourceBindingID: record.Input.Scope.SourceBindingID, Kind: "work-item", SourceID: record.Input.Request.SourceID}, Locator: workbench.SourceLocator{ID: record.Input.Request.ID}, Title: *record.Input.Request.Value, Revision: "revision-2"}
	}
	return receipt
}
func TestWorkbenchCommandCanonicalReplayAndExactHumanScope(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "commands.db"))
	record := acceptWorkbench(t, s, "same-key")
	duplicate, replayed, err := s.AcceptWorkbenchCommand(t.Context(), record.Input, childTestTime.Add(time.Minute))
	if err != nil || !replayed || duplicate.ID != record.ID || !duplicate.AcceptedAt.Equal(record.AcceptedAt) {
		t.Fatal(duplicate, replayed, err)
	}
	for _, change := range []func(*WorkbenchCommandInput){func(v *WorkbenchCommandInput) { v.TargetDigest = strings.Repeat("c", 64) }, func(v *WorkbenchCommandInput) { v.OperationDigest = strings.Repeat("c", 64) }, func(v *WorkbenchCommandInput) { title := "changed"; v.Request.Value = &title }} {
		input := record.Input
		change(&input)
		if _, _, err = s.AcceptWorkbenchCommand(t.Context(), input, childTestTime); !errors.Is(err, ErrConflict) {
			t.Fatal("changed command replayed", err)
		}
	}
	for _, change := range []func(*WorkbenchCommandScope){func(v *WorkbenchCommandScope) { v.Actor.Subject = "mallory" }, func(v *WorkbenchCommandScope) { v.Actor.Issuer = "https://other.example" }, func(v *WorkbenchCommandScope) { v.Gaggle = "other" }, func(v *WorkbenchCommandScope) { v.SourceBindingID = "other" }} {
		scope := record.Input.Scope
		change(&scope)
		if _, err = s.WorkbenchCommand(t.Context(), scope, record.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("foreign scope read receipt", err)
		}
	}
	input := record.Input
	input.Scope.Actor.Subject = "bob"
	separate, dup, err := s.AcceptWorkbenchCommand(t.Context(), input, childTestTime)
	if err != nil || dup || separate.ID == record.ID {
		t.Fatal("human identities collapsed", err)
	}
}
func TestWorkbenchCommandClaimIsAtomicAndNeverReplayedAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "commands.db")
	s := openTestStore(t, path)
	other := openTestStore(t, path)
	record := acceptWorkbench(t, s, "race")
	var claims atomic.Int32
	var group sync.WaitGroup
	for i := range 12 {
		group.Go(func() {
			store := s
			if i%2 == 1 {
				store = other
			}
			_, claimed, err := store.ClaimWorkbenchCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, childTestTime.Add(time.Second))
			if err != nil {
				t.Error(err)
			}
			if claimed {
				claims.Add(1)
			}
		})
	}
	group.Wait()
	if claims.Load() != 1 {
		t.Fatal("multiple external attempts admitted", claims.Load())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, path)
	got, claimed, err := reopened.ClaimWorkbenchCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, childTestTime.Add(time.Hour))
	if err != nil || claimed || got.State != "attempting" || got.AttemptedAt == nil {
		t.Fatal(got, claimed, err)
	}
}
func TestWorkbenchReceiptKeepsAcknowledgementSeparateFromObservedMatch(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "commands.db"))
	record := claimWorkbench(t, s, acceptWorkbench(t, s, "unknown"))
	receipt := workbenchReceipt(record, "confirmed")
	receipt.Outcome = "unknown"
	receipt.ProviderAcknowledged = false
	got, err := s.CompleteWorkbenchCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, receipt, childTestTime.Add(2*time.Second))
	if err != nil || got.State != "unknown" || got.Receipt.ProviderAcknowledged || !got.Receipt.ObservedMatches {
		t.Fatal(got, err)
	}
	if _, claimed, err := s.ClaimWorkbenchCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, childTestTime.Add(time.Hour)); err != nil || claimed {
		t.Fatal("unknown replayed", err)
	}
	repeated, err := s.CompleteWorkbenchCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, receipt, childTestTime.Add(time.Hour))
	if err != nil || !repeated.CompletedAt.Equal(*got.CompletedAt) {
		t.Fatal("receipt replay changed evidence", err)
	}
	receipt.ProviderAcknowledged = true
	receipt.Outcome = "confirmed"
	if _, err = s.CompleteWorkbenchCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, receipt, childTestTime.Add(time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatal("late observation rewrote outcome", err)
	}
}
func TestWorkbenchReceiptRefusesWrongIdentityAndFalseConfirmation(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "commands.db"))
	record := acceptWorkbench(t, s, "receipt")
	receipt := workbenchReceipt(record, "confirmed")
	if _, err := s.CompleteWorkbenchCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, receipt, childTestTime.Add(2*time.Second)); !errors.Is(err, ErrTransition) {
		t.Fatal("completed unclaimed effect", err)
	}
	record = claimWorkbench(t, s, record)
	for _, change := range []func(*workbench.BacklogPatchReceipt){func(v *workbench.BacklogPatchReceipt) { v.ProviderAcknowledged = false }, func(v *workbench.BacklogPatchReceipt) { v.Observed = nil }, func(v *workbench.BacklogPatchReceipt) { v.OperationDigest = strings.Repeat("f", 64) }, func(v *workbench.BacklogPatchReceipt) { v.Observed.Ref.SourceID = "foreign" }} {
		candidate := workbenchReceipt(record, "confirmed")
		change(&candidate)
		if _, err := s.CompleteWorkbenchCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, candidate, childTestTime.Add(2*time.Second)); err == nil {
			t.Fatal("invalid receipt accepted")
		}
	}
	got, err := s.CompleteWorkbenchCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, receipt, childTestTime.Add(2*time.Second))
	if err != nil || got.State != "confirmed" {
		t.Fatal(got, err)
	}
	var reserved int
	if err = s.db.QueryRow(`SELECT reserved_bytes FROM workbench_commands WHERE id=?`, record.ID).Scan(&reserved); err != nil || reserved != 0 {
		t.Fatal("receipt did not consume reservation", reserved, err)
	}
}
func TestWorkbenchCommandRetentionNeverExpiresUncertainEffects(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "commands.db"))
	accepted := acceptWorkbench(t, s, "accepted")
	attempting := claimWorkbench(t, s, acceptWorkbench(t, s, "attempting"))
	unknown := claimWorkbench(t, s, acceptWorkbench(t, s, "unknown"))
	confirmed := claimWorkbench(t, s, acceptWorkbench(t, s, "confirmed"))
	rejected := claimWorkbench(t, s, acceptWorkbench(t, s, "rejected"))
	for _, record := range []WorkbenchCommand{unknown, confirmed, rejected} {
		outcome := "unknown"
		if record.ID == confirmed.ID {
			outcome = "confirmed"
		}
		if record.ID == rejected.ID {
			outcome = "not-applied"
		}
		if _, err := s.CompleteWorkbenchCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, workbenchReceipt(record, outcome), childTestTime.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	now := childTestTime.Add(WorkbenchCommandRetention + time.Hour)
	if count, err := s.PruneWorkbenchCommands(t.Context(), now, 1); err != nil || count != 1 {
		t.Fatal("maintenance ignored batch bound", count, err)
	}
	if count, err := s.PruneWorkbenchCommands(t.Context(), now, 100); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	for _, record := range []WorkbenchCommand{confirmed, rejected} {
		got, dup, err := s.AcceptWorkbenchCommand(t.Context(), record.Input, now)
		if !errors.Is(err, ErrWorkbenchCommandExpired) || !dup || got.TombstonedAt == nil || got.ID != record.ID {
			t.Fatal("expired command readmitted", got, dup, err)
		}
	}
	if count, err := s.PruneWorkbenchCommands(t.Context(), now.Add(WorkbenchCommandRetention), 100); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	for _, record := range []WorkbenchCommand{accepted, attempting, unknown} {
		got, err := s.WorkbenchCommand(t.Context(), record.Input.Scope, record.ID)
		if err != nil || got.TombstonedAt != nil {
			t.Fatal("uncertain effect expired", got, err)
		}
	}
	// After the documented replay guarantee, only the host can submit a new
	// attempt after current identity/revision validation; this is not a retry.
	got, dup, err := s.AcceptWorkbenchCommand(t.Context(), confirmed.Input, now.Add(WorkbenchCommandRetention+time.Hour))
	if err != nil || dup || got.ID == confirmed.ID {
		t.Fatal(got, dup, err)
	}
}
func TestWorkbenchCommandsReserveSharedCapacityAndBoundPerGaggle(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "commands.db"))
	record := acceptWorkbench(t, s, "quota")
	var reserved int
	if err := s.db.QueryRow(`SELECT reserved_bytes FROM workbench_commands WHERE id=?`, record.ID).Scan(&reserved); err != nil || reserved != workbenchReceiptAllowance {
		t.Fatal(reserved, err)
	}
	// Model custody occupied by other unresolved commands. All queue callers
	// must see this reservation, even callers that never write workbench rows.
	if _, err := s.db.Exec(`UPDATE workbench_commands SET reserved_bytes=? WHERE id=?`, childStoreByteCeiling, record.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Accept(t.Context(), "ordinary", "actor", []byte(`{"workflow":"w"}`), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("ordinary intake spent workbench custody", err)
	}
	if _, _, err := s.AcceptWorkbenchCommand(t.Context(), workbenchInput("full"), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("command intake ignored shared bytes", err)
	}
	if _, dup, err := s.AcceptWorkbenchCommand(t.Context(), record.Input, childTestTime); err != nil || !dup {
		t.Fatal("full store rejected exact replay", err)
	}
	if _, err := s.db.Exec(`UPDATE workbench_commands SET reserved_bytes=0 WHERE id=?`, record.ID); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < MaxWorkbenchCommands; i++ {
		_, err := s.db.Exec(`INSERT INTO workbench_commands(id,key_digest,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,state,accepted_ns,reserved_bytes) SELECT ?,?,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,state,accepted_ns,0 FROM workbench_commands WHERE id=?`, fmt.Sprintf("test-record-%d", i), fmt.Sprintf("test-key-%d", i), record.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.AcceptWorkbenchCommand(t.Context(), workbenchInput("record-limit"), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("per-gaggle count exceeded", err)
	}
}

func TestWorkbenchCommandBoundsRefuseBeforeChangingCustody(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "commands.db"))
	input := workbenchInput("oversized")
	text := strings.Repeat("x", MaxWorkbenchRequestBytes+1)
	input.Request.Field = "description"
	input.Request.Value = &text
	if _, _, err := s.AcceptWorkbenchCommand(t.Context(), input, childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatal("oversized request admitted", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM workbench_commands`).Scan(&count); err != nil || count != 0 {
		t.Fatal("invalid input changed custody", count, err)
	}
	record := claimWorkbench(t, s, acceptWorkbench(t, s, "bounded"))
	receipt := workbenchReceipt(record, "confirmed")
	receipt.Observed.Description = strings.Repeat("x", workbench.MaxBacklogItemBytes)
	if _, err := s.CompleteWorkbenchCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, receipt, childTestTime.Add(2*time.Second)); !errors.Is(err, ErrTransition) {
		t.Fatal("oversized observation persisted", err)
	}
	retained, err := s.WorkbenchCommand(t.Context(), record.Input.Scope, record.ID)
	if err != nil || retained.State != "attempting" || retained.Receipt != nil {
		t.Fatal("failed receipt altered attempt custody", retained, err)
	}
	if _, err = s.CompleteWorkbenchCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, workbenchReceipt(record, "unknown"), childTestTime.Add(2*time.Second)); err != nil {
		t.Fatal("bounded fallback receipt could not be retained", err)
	}
}
