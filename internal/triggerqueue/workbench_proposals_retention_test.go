package triggerqueue

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

func TestWorkbenchProposalProductionMaintenanceBoundsAndPinsPartialEffects(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	acceptedInput, _ := proposalFixture(t, providers.ProviderGitHub, "accepted")
	accepted, _, err := s.AcceptWorkbenchProposal(t.Context(), acceptedInput, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	prepared := acceptProposal(t, s, providers.ProviderGitHub, "prepared")
	attempting := claimProposal(t, s, acceptProposal(t, s, providers.ProviderGitHub, "attempting"))
	unknown := claimProposal(t, s, acceptProposal(t, s, providers.ProviderGitHub, "unknown"))
	unknown = completeProposal(t, s, unknown, providers.RepositoryProposalPhaseResult{MutationAttempted: true})
	blocked := claimProposal(t, s, acceptProposal(t, s, providers.ProviderADO, "blocked"))
	blocked = completeProposal(t, s, blocked, proposalAck(blocked))
	blocked, err = s.StopWorkbenchProposal(t.Context(), blocked.Input.Scope, blocked.ID, blocked.RequestDigest, childTestTime.Add(time.Minute))
	if err != nil || blocked.State != "blocked" {
		t.Fatal(blocked, err)
	}
	stopped := acceptProposal(t, s, providers.ProviderGitHub, "stopped")
	stopped, err = s.StopWorkbenchProposal(t.Context(), stopped.Input.Scope, stopped.ID, stopped.RequestDigest, childTestTime.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	confirmed := acceptProposal(t, s, providers.ProviderGitHub, "confirmed")
	for confirmed.State == "prepared" {
		confirmed = claimProposal(t, s, confirmed)
		confirmed = completeProposal(t, s, confirmed, proposalAck(confirmed))
	}
	// The existing daemon maintenance entrypoint includes the new proposal kind.
	now := childTestTime.Add(WorkbenchCommandRetention + time.Hour)
	if count, err := s.PruneWorkbenchCommands(t.Context(), now, 1); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if count, err := s.PruneWorkbenchCommands(t.Context(), now, 100); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	for _, r := range []WorkbenchProposal{stopped, confirmed} {
		got, dup, err := s.AcceptWorkbenchProposal(t.Context(), r.Input, now)
		if !errors.Is(err, ErrWorkbenchCommandExpired) || !dup || got.ID != r.ID || got.TombstonedAt == nil || got.Plan != nil {
			t.Fatal(got, dup, err)
		}
		var bytes int
		if err := s.db.QueryRow(`SELECT length(request)+length(plan)+length(before_source)+length(after_source)+length(history)+reserved_bytes FROM workbench_proposals WHERE id=?`, r.ID).Scan(&bytes); err != nil || bytes != 0 {
			t.Fatal(bytes, err)
		}
	}
	if count, err := s.PruneWorkbenchCommands(t.Context(), now.Add(WorkbenchCommandRetention), 100); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	for _, r := range []WorkbenchProposal{accepted, prepared, attempting, unknown, blocked} {
		got, err := s.WorkbenchProposal(t.Context(), r.Input.Scope, r.ID)
		if err != nil || got.State != r.State {
			t.Fatal("unresolved custody expired", got, err)
		}
	}
}
func TestWorkbenchProposalSharesNativeCountAndGlobalByteReservation(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	r := acceptProposal(t, s, providers.ProviderGitHub, "reservation")
	if _, err := s.db.Exec(`UPDATE workbench_proposals SET reserved_bytes=? WHERE id=?`, childStoreByteCeiling, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Accept(t.Context(), "ordinary", "actor", []byte(`{"workflow":"w"}`), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("ordinary spent proposal reserve", err)
	}
	if _, _, err := s.AcceptWorkbenchCommand(t.Context(), workbenchInput("full"), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("native ignored proposal reserve", err)
	}
	if _, dup, err := s.AcceptWorkbenchProposal(t.Context(), r.Input, childTestTime); err != nil || !dup {
		t.Fatal("full store denied replay", err)
	}
	if _, err := s.db.Exec(`UPDATE workbench_proposals SET reserved_bytes=0 WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	native := acceptWorkbench(t, s, "count")
	acceptNeedsHuman(t, s, "count-resolution")
	for i := 3; i < MaxWorkbenchCommands; i++ {
		_, err := s.db.Exec(`INSERT INTO workbench_commands(id,key_digest,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,state,accepted_ns,reserved_bytes) SELECT ?,?,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,state,accepted_ns,0 FROM workbench_commands WHERE id=?`, fmt.Sprintf("count-%d", i), fmt.Sprintf("key-%d", i), native.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	next, _ := proposalFixture(t, providers.ProviderGitHub, "next")
	if _, _, err := s.AcceptWorkbenchProposal(t.Context(), next, childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("proposal ignored combined count", err)
	}
	if _, _, err := s.AcceptWorkbenchCommand(t.Context(), workbenchInput("next"), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("native ignored combined count", err)
	}
	if _, _, err := s.AcceptNeedsHumanCommand(t.Context(), needsHumanInput("count-full"), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("resolution ignored combined count", err)
	}
}
func TestWorkbenchProposalMigrationPreservesExistingNativeCustody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	version := 0
	for _, migration := range migrations {
		if migration == workbenchProposalSchema {
			break
		}
		if _, err = db.Exec(migration); err != nil {
			t.Fatal(err)
		}
		version++
	}
	if _, err = db.Exec(`CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES(?)`, version); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES('legacy','legacy','actor',?,'accepted',?)`, []byte(`{"workflow":"w"}`), childTestTime.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, path)
	if _, err = s.Get(t.Context(), "legacy", "actor"); err != nil {
		t.Fatal(err)
	}
	acceptProposal(t, s, providers.ProviderGitHub, "migrated")
}

func TestWorkbenchMaintenanceGivesEveryKindShareUnderNativeBacklog(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	native := claimWorkbench(t, s, acceptWorkbench(t, s, "native"))
	if _, err := s.CompleteWorkbenchCommand(t.Context(), native.Input.Scope, native.ID, native.RequestDigest, workbenchReceipt(native, "confirmed"), childTestTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for i := range 99 {
		if _, err := s.db.Exec(`INSERT INTO workbench_commands(id,key_digest,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,state,accepted_ns,completed_ns,reserved_bytes) SELECT ?,?,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,state,accepted_ns,completed_ns,0 FROM workbench_commands WHERE id=?`, fmt.Sprintf("native-%d", i), fmt.Sprintf("key-%d", i), native.ID); err != nil {
			t.Fatal(err)
		}
	}
	proposal := acceptProposal(t, s, providers.ProviderGitHub, "proposal")
	if _, err := s.StopWorkbenchProposal(t.Context(), proposal.Input.Scope, proposal.ID, proposal.RequestDigest, childTestTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	resolution := acceptNeedsHuman(t, s, "settled-resolution")
	resolution, claimed, err := s.ClaimNeedsHumanCommand(t.Context(), resolution.Input.Scope, resolution.ID, resolution.RequestDigest, childTestTime.Add(time.Second))
	if err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	if _, err := s.CompleteNeedsHumanCommand(t.Context(), resolution.Input.Scope, resolution.ID, resolution.RequestDigest, resolutionReceipt(resolution, "confirmed"), childTestTime.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneWorkbenchCommands(t.Context(), childTestTime.Add(WorkbenchCommandRetention+time.Hour), 100); err != nil || n != 36 {
		t.Fatal(n, err)
	}
	if _, err := s.WorkbenchProposal(t.Context(), proposal.Input.Scope, proposal.ID); !errors.Is(err, ErrWorkbenchCommandExpired) {
		t.Fatal("busy native kind starved proposals", err)
	}
	if _, err := s.NeedsHumanCommand(t.Context(), resolution.Input.Scope, resolution.ID); !errors.Is(err, ErrWorkbenchCommandExpired) {
		t.Fatal("busy native kind starved resolution", err)
	}
}

func TestWorkbenchProposalCannotSpendNeedsHumanByteReservation(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	resolution := acceptNeedsHuman(t, s, "reserved-resolution")
	if _, err := s.db.Exec(`UPDATE needs_human_commands SET reserved_bytes=? WHERE id=?`, childStoreByteCeiling, resolution.ID); err != nil {
		t.Fatal(err)
	}
	input, _ := proposalFixture(t, providers.ProviderGitHub, "denied")
	if _, _, err := s.AcceptWorkbenchProposal(t.Context(), input, childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("proposal spent resolution receipt reserve", err)
	}
}
