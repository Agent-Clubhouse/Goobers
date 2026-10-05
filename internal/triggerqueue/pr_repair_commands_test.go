package triggerqueue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

func repairInputFixture(t *testing.T, s *Store) PRRepairCommandInput {
	t.Helper()
	conversation := createSessionTest(t, s, "create")
	selection := sessionRepairFixture()
	accepted, err := s.SubmitSessionInput(t.Context(), sessionCommand("human"), conversation.ID, sessioning.MessageRequest{Text: "Repair this selected PR", RepairTarget: selection}, []byte(`{}`), childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := s.SessionInputs(t.Context(), accepted.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BeginSessionTurn(t.Context(), accepted.AcceptanceID, childTestTime); err != nil {
		t.Fatal(err)
	}
	run := strings.TrimPrefix(accepted.AcceptanceID, "trigger-")
	if err = s.ObserveSessionRun(t.Context(), accepted.AcceptanceID, run, childTestTime); err != nil {
		t.Fatal(err)
	}
	turn, err := s.SessionTurn(t.Context(), accepted.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := inputs.Validate(run, "gaggle")
	if err != nil {
		t.Fatal(err)
	}
	content := "fixed\n"
	input := PRRepairCommandInput{Scope: WorkbenchCommandScope{Gaggle: "gaggle", SourceBindingID: selection.SourceBindingID, Actor: *turn.Message.Actor}, RequestID: "repair-one", Selection: *selection, Request: sessioning.PRRepairRequest{RequestID: "model-one", ExpectedHeadSHA: selection.ExpectedHeadSHA, Rationale: "Fix the reported issue", Changes: []sessioning.PRRepairChange{{Path: "file.txt", PreviousBlob: strings.Repeat("b", 40), Content: &content}}}, Origin: sessioning.PRRepairOrigin{RunID: run, SessionID: conversation.ID, TurnID: turn.ID, MessageID: turn.Message.ID, MessageDigest: inputs.Start.MessageDigest, ConfigGeneration: turn.Session.ConfigGeneration, GooberDigest: turn.Session.GooberDigest, EnvelopeDigest: sessioning.Digest(turn.Record.Payload), InputDigest: sessioning.Digest(raw)}}
	input.Target = &providers.RepairPullRequest{Repository: repairRepository(*selection), RepositoryID: selection.RepositorySourceID, ID: selection.ID, StableID: selection.SourceID, Head: "fix", Base: "main", HeadSHA: selection.ExpectedHeadSHA, BaseSHA: strings.Repeat("f", 40), Open: true}
	setRepairDigests(t, &input)
	return input
}
func setRepairDigests(t *testing.T, input *PRRepairCommandInput) {
	t.Helper()
	var err error
	input.TargetDigest, input.OperationDigest, err = PRRepairDigests(input.Scope, input.Selection, input.Request)
	if err != nil {
		t.Fatal(err)
	}
}
func repairAccept(t *testing.T, s *Store, input PRRepairCommandInput) PRRepairCommand {
	t.Helper()
	record, dup, err := s.AcceptPRRepairCommand(t.Context(), input, childTestTime)
	if err != nil || dup {
		t.Fatal(record, dup, err)
	}
	return record
}
func repairComplete(t *testing.T, s *Store, record PRRepairCommand, outcome string) PRRepairCommand {
	t.Helper()
	claimed, ok, err := s.ClaimPRRepairCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, childTestTime)
	if err != nil || !ok {
		t.Fatal(claimed, ok, err)
	}
	receipt := sessioning.PRRepairReceipt{OperationDigest: record.Input.OperationDigest, Outcome: outcome}
	if outcome == "confirmed" {
		receipt.MutationAttempted, receipt.ProviderAcknowledged, receipt.ObservedMatches = true, true, true
		receipt.CommitID = strings.Repeat("c", 40)
	}
	if outcome == "unknown" {
		receipt.MutationAttempted, receipt.ObservedMatches = true, true
		receipt.CommitID = strings.Repeat("c", 40)
	}
	if receipt.CommitID == record.Input.Request.ExpectedHeadSHA {
		receipt.CommitID = strings.Repeat("d", 40)
	}
	record, err = s.CompletePRRepairCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, receipt, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	return record
}
func TestPRRepairCustodySingleAttemptAcrossStoresAndUnknownReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openTestStore(t, path)
	other := openTestStore(t, path)
	input := repairInputFixture(t, s)
	record := repairAccept(t, s, input)
	var winners atomic.Int32
	var wg sync.WaitGroup
	for _, store := range []*Store{s, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, claimed, err := store.ClaimPRRepairCommand(t.Context(), input.Scope, record.ID, record.RequestDigest, childTestTime)
			if err != nil {
				t.Error(err)
			}
			if claimed {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("duplicate provider claim", winners.Load())
	}
	receipt := sessioning.PRRepairReceipt{OperationDigest: input.OperationDigest, Outcome: "unknown", MutationAttempted: true, ObservedMatches: true, CommitID: strings.Repeat("c", 40)}
	if _, err := other.CompletePRRepairCommand(t.Context(), input.Scope, record.ID, record.RequestDigest, receipt, childTestTime); err != nil {
		t.Fatal(err)
	}
	replayInput := input
	replayInput.Target = nil
	replay, dup, err := s.AcceptPRRepairCommand(t.Context(), replayInput, childTestTime)
	if err != nil || !dup || replay.State != "unknown" || replay.Receipt.ProviderAcknowledged {
		t.Fatal(replay, dup, err)
	}
	if _, claimed, err := s.ClaimPRRepairCommand(t.Context(), input.Scope, record.ID, record.RequestDigest, childTestTime); err != nil || claimed {
		t.Fatal("unknown reattempt", claimed, err)
	}
	receipt.Outcome, receipt.ProviderAcknowledged = "confirmed", true
	if _, err = s.CompletePRRepairCommand(t.Context(), input.Scope, record.ID, record.RequestDigest, receipt, childTestTime); !errors.Is(err, ErrConflict) {
		t.Fatal("later read rewrote unknown receipt", err)
	}
	changed := input
	changed.Request.Rationale = "Different intent"
	setRepairDigests(t, &changed)
	if _, _, err = s.AcceptPRRepairCommand(t.Context(), changed, childTestTime); !errors.Is(err, ErrConflict) {
		t.Fatal("changed intent reused key", err)
	}
}
func TestPRRepairCustodyRequiresActualSelectedPublishedTurn(t *testing.T) {
	for _, mode := range []string{"actor", "origin", "input", "selection", "target", "head", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
			input := repairInputFixture(t, s)
			switch mode {
			case "actor":
				input.Scope.Actor.Subject = "bob"
			case "origin":
				input.Origin.RunID = strings.Repeat("9", 32)
			case "input":
				input.Origin.InputDigest = "sha256:" + strings.Repeat("9", 64)
			case "selection":
				input.Selection.SourceID = "901"
				setRepairDigests(t, &input)
			case "target":
				input.Target.RepositoryID = "101"
			case "head":
				input.Target.HeadSHA = strings.Repeat("e", 40)
			case "canceled":
				if _, err := s.CloseSession(t.Context(), sessionCommand("close"), input.Origin.SessionID, "stop", childTestTime); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := s.AcceptPRRepairCommand(t.Context(), input, childTestTime); err == nil {
				t.Fatal("unbound repair accepted")
			}
		})
	}
}
func TestPRRepairCustodyIterationRequiresConfirmedSameTurnDescendant(t *testing.T) {
	for _, outcome := range []string{"confirmed", "unknown", "not-applied"} {
		t.Run(outcome, func(t *testing.T) {
			s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
			input := repairInputFixture(t, s)
			first := repairComplete(t, s, repairAccept(t, s, input), outcome)
			next := input
			next.RequestID = "repair-two"
			next.Request.RequestID = "model-two"
			next.Request.ParentCommandID = first.ID
			next.Request.ExpectedHeadSHA = strings.Repeat("c", 40)
			target := *input.Target
			target.HeadSHA = next.Request.ExpectedHeadSHA
			next.Target = &target
			setRepairDigests(t, &next)
			record, _, err := s.AcceptPRRepairCommand(t.Context(), next, childTestTime)
			if outcome != "confirmed" {
				if err == nil {
					t.Fatal("unconfirmed descendant advanced", record)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			native, err := record.Native()
			if err != nil || native.Target.HeadSHA != first.Receipt.CommitID || !strings.Contains(native.Message, "Goobers-Repair: "+record.ID+"\n") || record.Input.Selection.ExpectedHeadSHA != input.Selection.ExpectedHeadSHA {
				t.Fatal(native, err)
			}
			next.RequestID = "foreign-head"
			next.Request.ExpectedHeadSHA = strings.Repeat("e", 40)
			next.Target.HeadSHA = next.Request.ExpectedHeadSHA
			setRepairDigests(t, &next)
			if _, _, err = s.AcceptPRRepairCommand(t.Context(), next, childTestTime); err == nil {
				t.Fatal("foreign movement became repair authority")
			}
		})
	}
}
func TestPRRepairCustodyPinsClosedSessionRunGenerationAndAncestor(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	input := repairInputFixture(t, s)
	first := repairComplete(t, s, repairAccept(t, s, input), "confirmed")
	next := input
	next.RequestID = "repair-two"
	next.Request.ParentCommandID = first.ID
	next.Request.ExpectedHeadSHA = first.Receipt.CommitID
	target := *input.Target
	target.HeadSHA = next.Request.ExpectedHeadSHA
	next.Target = &target
	setRepairDigests(t, &next)
	unknown := repairComplete(t, s, repairAccept(t, s, next), "unknown")
	if err := s.CompleteSessionTurn(t.Context(), "trigger-"+input.Origin.RunID, SessionCompletion{RunID: input.Origin.RunID, Outcome: "success", Text: "repair pending"}, childTestTime); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseSession(t.Context(), sessionCommand("close"), input.Origin.SessionID, "done", childTestTime); err != nil {
		t.Fatal(err)
	}
	later := childTestTime.Add(365 * 24 * time.Hour)
	if _, err := s.PruneWorkbenchCommands(t.Context(), later, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneSessions(t.Context(), later, 100); err != nil {
		t.Fatal(err)
	}
	if retained, err := s.SessionRunRetained(t.Context(), input.Origin.RunID); err != nil || !retained {
		t.Fatal("unknown source run pruned", retained, err)
	}
	pins, err := s.RetainedSessionGenerations(t.Context())
	if err != nil || len(pins) != 1 || pins[0] != input.Origin.ConfigGeneration {
		t.Fatal("unknown config source pruned", pins, err)
	}
	if prior, err := s.PRRepairCommand(t.Context(), input.Scope, first.ID); err != nil || prior.State != "confirmed" {
		t.Fatal("uncertain descendant lost ancestor", prior, err)
	}
	if _, err := s.SessionInputs(t.Context(), "trigger-"+input.Origin.RunID); err != nil {
		t.Fatal("unknown original input pruned", err)
	}
	if _, err := s.db.Exec(`DELETE FROM interactive_turns WHERE id=?`, input.Origin.TurnID); err == nil {
		t.Fatal("FK did not protect uncertain turn", unknown.ID)
	}
}
func TestPRRepairCustodySettledReleaseAndTombstoneWindow(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	input := repairInputFixture(t, s)
	record := repairComplete(t, s, repairAccept(t, s, input), "not-applied")
	if err := s.CompleteSessionTurn(t.Context(), "trigger-"+input.Origin.RunID, SessionCompletion{RunID: input.Origin.RunID, Outcome: "success", Text: "not applied"}, childTestTime); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseSession(t.Context(), sessionCommand("close"), input.Origin.SessionID, "done", childTestTime); err != nil {
		t.Fatal(err)
	}
	later := childTestTime.Add(31 * 24 * time.Hour)
	if n, err := s.PrunePRRepairCommands(t.Context(), later, 100); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, err := s.PRRepairCommand(t.Context(), input.Scope, record.ID); !errors.Is(err, ErrWorkbenchCommandExpired) {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptPRRepairCommand(t.Context(), input, later); !errors.Is(err, ErrWorkbenchCommandExpired) {
		t.Fatal("tombstone replay", err)
	}
	if _, err := s.PruneSessions(t.Context(), later, 100); err != nil {
		t.Fatal(err)
	}
	if retained, err := s.SessionRunRetained(t.Context(), input.Origin.RunID); err != nil || retained {
		t.Fatal("settled custody never released", retained, err)
	}
	if _, err := s.PrunePRRepairCommands(t.Context(), later.Add(31*24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PRRepairCommand(t.Context(), input.Scope, record.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
}
func TestPRRepairCustodyRejectsAlteredCanonicalEvidence(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	input := repairInputFixture(t, s)
	record := repairAccept(t, s, input)
	target := *input.Target
	target.RepositoryID = "101"
	raw, _ := json.Marshal(target)
	if _, err := s.db.Exec(`UPDATE pr_repair_commands SET evidence=?,evidence_digest=? WHERE id=?`, raw, childDigest(raw), record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PRRepairCommand(t.Context(), input.Scope, record.ID); err == nil {
		t.Fatal("changed evidence retained authority")
	}
}

func TestPRRepairCustodySharesAllCommandCountAndGlobalReservation(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	input := repairInputFixture(t, s)
	record := repairAccept(t, s, input)
	if _, err := s.db.Exec(`UPDATE pr_repair_commands SET reserved_bytes=? WHERE id=?`, childStoreByteCeiling, record.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Accept(t.Context(), "ordinary", "actor", []byte(`{"workflow":"w"}`), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("ordinary ignored repair reserve", err)
	}
	if _, _, err := s.AcceptWorkbenchCommand(t.Context(), workbenchInput("full"), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("native ignored repair reserve", err)
	}
	if _, dup, err := s.AcceptPRRepairCommand(t.Context(), input, childTestTime); err != nil || !dup {
		t.Fatal("replay required new quota", dup, err)
	}
	if _, err := s.db.Exec(`UPDATE pr_repair_commands SET reserved_bytes=? WHERE id=?`, prRepairReceiptAllowance, record.ID); err != nil {
		t.Fatal(err)
	}
	repairComplete(t, s, record, "not-applied")
	nativeInput := workbenchInput("native")
	nativeInput.Scope.Gaggle = input.Scope.Gaggle
	native, _, err := s.AcceptWorkbenchCommand(t.Context(), nativeInput, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 2 UNION ALL SELECT i+1 FROM n WHERE i<?) INSERT INTO workbench_commands(id,key_digest,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,state,accepted_ns,reserved_bytes) SELECT 'count-'||i,'count-key-'||i,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,state,accepted_ns,0 FROM n CROSS JOIN workbench_commands WHERE id=?`, MaxWorkbenchCommands-1, native.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := input
	next.RequestID = "next"
	if _, _, err = s.AcceptPRRepairCommand(t.Context(), next, childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("repair ignored combined count", err)
	}
	if _, _, err = s.AcceptWorkbenchCommand(t.Context(), func() WorkbenchCommandInput {
		v := workbenchInput("next")
		v.Scope.Gaggle = input.Scope.Gaggle
		return v
	}(), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("native ignored combined count", err)
	}
	proposal, _ := proposalFixture(t, providers.ProviderGitHub, "next")
	proposal.Scope.Gaggle = input.Scope.Gaggle
	if _, _, err = s.AcceptWorkbenchProposal(t.Context(), proposal, childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("proposal ignored combined count", err)
	}
	if _, _, err = s.AcceptNeedsHumanCommand(t.Context(), func() NeedsHumanCommandInput {
		v := needsHumanInput("next")
		v.Scope.Gaggle = input.Scope.Gaggle
		v.Observation.Item.Ref.GaggleID = input.Scope.Gaggle
		v.Observation.Digest, _ = workbench.NeedsHumanObservationDigest(*v.Observation)
		v.Request.ObservationDigest = v.Observation.Digest
		return v
	}(), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("resolution ignored combined count", err)
	}
}

func TestPRRepairCustodyUncertaintyBlocksNewKeysAndConfiguredAliases(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	input := repairInputFixture(t, s)
	record := repairComplete(t, s, repairAccept(t, s, input), "unknown")
	next := input
	next.RequestID = "new-key"
	if _, _, err := s.AcceptPRRepairCommand(t.Context(), next, childTestTime); !errors.Is(err, ErrConflict) {
		t.Fatal("new key bypassed uncertainty", err)
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	alias := input.Selection
	alias.SourceBindingID = "another-binding"
	alias.Repository.Name = "renamed"
	alias.Repository.Owner = strings.ToUpper(alias.Repository.Owner)
	if err = verifyPRRepairWriterCustody(t.Context(), tx, alias); !errors.Is(err, ErrConflict) {
		t.Fatal("configured alias lost native writer custody", record, err)
	}
}
func TestPRRepairCustodyStopsOnlyUnclaimedIntentAfterTurnCancellation(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	input := repairInputFixture(t, s)
	record := repairAccept(t, s, input)
	if _, err := s.CloseSession(t.Context(), sessionCommand("close"), input.Origin.SessionID, "stop", childTestTime); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimPRRepairCommand(t.Context(), input.Scope, record.ID, record.RequestDigest, childTestTime); err == nil || claimed {
		t.Fatal("canceled turn acquired effect", claimed, err)
	}
	stopped, err := s.StopPRRepairCommand(t.Context(), input.Scope, record.ID, record.RequestDigest, childTestTime)
	if err != nil || stopped.State != "not-applied" || stopped.AttemptedAt != nil || stopped.Receipt.MutationAttempted {
		t.Fatal(stopped, err)
	}
	if _, err = s.StopPRRepairCommand(t.Context(), input.Scope, record.ID, record.RequestDigest, childTestTime); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PrunePRRepairCommands(t.Context(), childTestTime.Add(31*24*time.Hour), 100); err != nil || n != 1 {
		t.Fatal("unused custody never released", n, err)
	}
	other := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	input = repairInputFixture(t, other)
	uncertain := repairComplete(t, other, repairAccept(t, other, input), "unknown")
	got, err := other.StopPRRepairCommand(t.Context(), input.Scope, uncertain.ID, uncertain.RequestDigest, childTestTime)
	if err != nil || got.State != "unknown" || !got.Receipt.MutationAttempted {
		t.Fatal("stop relabeled uncertain effect", got, err)
	}
}
