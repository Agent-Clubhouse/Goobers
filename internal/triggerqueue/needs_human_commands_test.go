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

	"github.com/goobers/goobers/internal/workbench"
)

func needsHumanInput(key string) NeedsHumanCommandInput {
	native := workbenchInput(key)
	origin := workbench.NeedsHumanResolutionOrigin{RunID: strings.Repeat("a", 32), SessionID: "session-one", TurnID: "turn-one", MessageID: "message-one", MessageDigest: strings.Repeat("c", 64), GooberDigest: "sha256:" + strings.Repeat("d", 64)}
	observation := workbench.NeedsHumanObservation{Item: workbench.BacklogItem{Ref: workbench.NodeRef{GaggleID: native.Scope.Gaggle, SourceBindingID: native.Scope.SourceBindingID, Kind: "work-item", SourceID: "9876"}, Locator: workbench.SourceLocator{ID: "12"}, Revision: "revision-1", Labels: []string{"goobers:needs-human"}}, MarkerPresent: true, CommentsComplete: true, DependenciesComplete: true, LearnedComplete: true, LearnedRecordDigest: strings.Repeat("e", 64)}
	observation.Digest, _ = workbench.NeedsHumanObservationDigest(observation)
	basis := workbench.NeedsHumanEvidenceRef{Kind: "current-human-message", ID: origin.MessageID, Digest: origin.MessageDigest}
	return NeedsHumanCommandInput{Scope: native.Scope, RequestID: key, TargetDigest: native.TargetDigest, OperationDigest: native.OperationDigest, Origin: origin, Observation: &observation, Request: workbench.NeedsHumanResolutionRequest{ID: "12", SourceID: "9876", ExpectedRevision: "revision-1", ObservationDigest: observation.Digest, Basis: basis, Rationale: "The current answer resolves the recorded question.\nKnown dependency inventory is complete.", Evidence: []workbench.NeedsHumanEvidenceRef{basis}}}
}
func acceptNeedsHuman(t *testing.T, s *Store, key string) NeedsHumanCommand {
	t.Helper()
	record, dup, err := s.AcceptNeedsHumanCommand(t.Context(), needsHumanInput(key), childTestTime)
	if err != nil || dup {
		t.Fatal(record, dup, err)
	}
	return record
}
func resolutionReceipt(record NeedsHumanCommand, outcome string) workbench.NeedsHumanResolutionReceipt {
	receipt := workbench.NeedsHumanResolutionReceipt{OperationDigest: record.Input.OperationDigest, Outcome: outcome, RevisionSemantics: "timestamp-preflight"}
	if outcome == "confirmed" {
		observed := record.Input.Observation.Item
		observed.Labels = nil
		observed.Revision = "revision-2"
		receipt.Observed = &observed
		receipt.ProviderAcknowledged = true
		receipt.ObservedClear = true
	}
	return receipt
}
func TestNeedsHumanCustodyRefusesIncompleteOrFabricatedBasis(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	for _, mode := range []string{"coverage", "open-dependency", "unknown-dependency", "human-id", "human-digest", "missing-reason", "changed-item"} {
		t.Run(mode, func(t *testing.T) {
			input := needsHumanInput(mode)
			switch mode {
			case "coverage":
				input.Observation.CommentsComplete = false
			case "open-dependency":
				input.Observation.Dependencies = []workbench.NeedsHumanDependency{{ID: "other", Open: true, Verified: true}}
			case "unknown-dependency":
				input.Observation.LearnedDependencies = []workbench.NeedsHumanDependency{{ID: "other", Verified: false}}
			case "human-id":
				input.Request.Basis.ID = "some-other-message"
			case "human-digest":
				input.Request.Basis.Digest = strings.Repeat("f", 64)
			case "missing-reason":
				input.Request.Basis.Kind = "learned-record"
				input.Request.Basis.ID = input.Request.ID
				input.Request.Basis.Digest = input.Observation.LearnedRecordDigest
			case "changed-item":
				input.Observation.Item.Ref.SourceID = "foreign"
			}
			input.Observation.Digest, _ = workbench.NeedsHumanObservationDigest(*input.Observation)
			input.Request.ObservationDigest = input.Observation.Digest
			if _, _, err := s.AcceptNeedsHumanCommand(t.Context(), input, childTestTime); err == nil {
				t.Fatal("unsafe acceptance")
			}
		})
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM needs_human_commands`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
func TestNeedsHumanCustodyClaimsOnceAndReplaysWithoutFreshObservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openTestStore(t, path)
	record := acceptNeedsHuman(t, s, "same")
	reopened := openTestStore(t, path)
	var claims atomic.Int32
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := s
			if i%2 == 0 {
				store = reopened
			}
			_, won, err := store.ClaimNeedsHumanCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, childTestTime.Add(time.Second))
			if err != nil {
				t.Error(err)
			}
			if won {
				claims.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatal("multiple effect owners", claims.Load())
	}
	confirmed, err := s.CompleteNeedsHumanCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, resolutionReceipt(record, "confirmed"), childTestTime.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	input := record.Input
	input.Observation = nil
	replay, dup, err := s.AcceptNeedsHumanCommand(t.Context(), input, childTestTime.Add(time.Minute))
	if err != nil || !dup || replay.ID != confirmed.ID || replay.State != "confirmed" {
		t.Fatal(replay, dup, err)
	}
	input.Request.Rationale = "Different rationale"
	if _, _, err = s.AcceptNeedsHumanCommand(t.Context(), input, childTestTime); !errors.Is(err, ErrConflict) {
		t.Fatal("changed assessment reused key", err)
	}
	scope := record.Input.Scope
	scope.Actor.Subject = "other"
	if _, err = s.NeedsHumanCommand(t.Context(), scope, record.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign human read evidence", err)
	}
}
func TestNeedsHumanCustodyUnknownNeverExpiresOrPromotes(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	record := acceptNeedsHuman(t, s, "unknown")
	if _, _, err := s.ClaimNeedsHumanCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, childTestTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteNeedsHumanCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, resolutionReceipt(record, "unknown"), childTestTime.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteNeedsHumanCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, resolutionReceipt(record, "confirmed"), childTestTime.Add(3*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatal("uncertain effect promoted", err)
	}
	if _, err := s.PruneNeedsHumanCommands(t.Context(), childTestTime.Add(365*24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	got, won, err := s.ClaimNeedsHumanCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, childTestTime.Add(366*24*time.Hour))
	if err != nil || won || got.State != "unknown" {
		t.Fatal(got, won, err)
	}
}
func TestNeedsHumanCustodySharesNativeCountAndByteReservations(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	record := acceptNeedsHuman(t, s, "shared")
	if _, err := s.db.Exec(`UPDATE needs_human_commands SET reserved_bytes=? WHERE id=?`, childStoreByteCeiling, record.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptWorkbenchCommand(t.Context(), workbenchInput("full"), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("native command ignored resolution reservation", err)
	}
	if _, err := s.db.Exec(`UPDATE needs_human_commands SET reserved_bytes=0 WHERE id=?`, record.ID); err != nil {
		t.Fatal(err)
	}
	native := acceptWorkbench(t, s, "native")
	for i := 2; i < MaxWorkbenchCommands; i++ {
		_, err := s.db.Exec(`INSERT INTO workbench_commands(id,key_digest,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,state,accepted_ns,reserved_bytes) SELECT ?,?,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,state,accepted_ns,0 FROM workbench_commands WHERE id=?`, fmt.Sprintf("fixture-%d", i), fmt.Sprintf("fixture-key-%d", i), native.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.AcceptNeedsHumanCommand(t.Context(), needsHumanInput("full-count"), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("resolution kind bypassed shared count", err)
	}
	if _, _, err := s.AcceptWorkbenchCommand(t.Context(), workbenchInput("full-count"), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("native kind bypassed shared count", err)
	}
}

func TestNeedsHumanCustodyRetainsTombstoneAndRejectsAlteredEvidence(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	record := acceptNeedsHuman(t, s, "settled")
	if _, _, err := s.ClaimNeedsHumanCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, childTestTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteNeedsHumanCommand(t.Context(), record.Input.Scope, record.ID, record.RequestDigest, resolutionReceipt(record, "confirmed"), childTestTime.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	tombstoneAt := childTestTime.Add(31 * 24 * time.Hour)
	if count, err := s.PruneNeedsHumanCommands(t.Context(), tombstoneAt, 1); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	got, err := s.FindNeedsHumanCommand(t.Context(), record.Input.Scope, record.Input.RequestID)
	if !errors.Is(err, ErrWorkbenchCommandExpired) || got.Input.Observation != nil || got.Receipt != nil || got.State != "tombstoned" {
		t.Fatal(got, err)
	}
	input := record.Input
	input.Observation = nil
	if _, dup, err := s.AcceptNeedsHumanCommand(t.Context(), input, tombstoneAt); !dup || !errors.Is(err, ErrWorkbenchCommandExpired) {
		t.Fatal(dup, err)
	}
	if count, err := s.PruneNeedsHumanCommands(t.Context(), tombstoneAt.Add(WorkbenchCommandRetention), 1); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if _, _, err := s.AcceptNeedsHumanCommand(t.Context(), input, tombstoneAt.Add(WorkbenchCommandRetention)); err == nil {
		t.Fatal("expired key accepted without new verified observation")
	}
	corrupt := acceptNeedsHuman(t, s, "corrupt")
	if _, err := s.db.Exec(`UPDATE needs_human_commands SET evidence='{}' WHERE id=?`, corrupt.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimNeedsHumanCommand(t.Context(), corrupt.Input.Scope, corrupt.ID, corrupt.RequestDigest, childTestTime.Add(time.Second)); err == nil {
		t.Fatal("tampered evidence granted effect custody")
	}
}
