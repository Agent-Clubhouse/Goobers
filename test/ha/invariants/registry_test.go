package invariants

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/goobers/goobers/internal/cloud/lease"
)

// injections maps each checkable invariant to statements that violate it and
// (as far as the schema allows) only it. Statements run in a rolled-back
// transaction on top of cleanData.
var injections = map[string][][]string{
	"no-duplicate-emit-key": {{
		`DROP INDEX journal.run_event_emit_key`,
		`INSERT INTO journal.run_event (instance_id, run_id, seq, type, emit_key, at, body) VALUES ('i','r1',3,'t','k1','2026-01-01 00:03:00+00','\x00')`,
		`UPDATE journal.run_seq SET last_seq = 3 WHERE run_id = 'r1'`,
	}},
	"journal-seq-contiguous": {{
		`INSERT INTO journal.run_event (instance_id, run_id, seq, type, at, body) VALUES ('i','r2',3,'t','2026-01-01 00:03:00+00','\x00')`,
		`UPDATE journal.run_seq SET last_seq = 3 WHERE run_id = 'r2'`,
	}},
	"no-claim-with-two-runs": {{
		`INSERT INTO control.claim_run_history (instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,claimed_at,renewed_at,expires_at,released_at,release_reason,verification) VALUES ('i','g','gh','42','r4','w','tok4',1,'2026-01-01 00:02:00+00','2026-01-01 00:02:00+00','2026-01-01 00:03:00+00','2026-01-01 00:04:00+00','{}',NULL)`,
	}},
	"one-acceptance-per-source-key": {{
		`INSERT INTO control.source_receipt VALUES ('i','src3','act','fp',ARRAY['a1'],'2026-01-01 00:01:00+00')`,
	}},
	"no-duplicate-run-start": {{
		`DROP INDEX control.acceptance_run_idx`,
		`UPDATE control.acceptance SET run_id = 'r1' WHERE id = 'a2'`,
	}},
	"no-lost-acceptance": {{
		`UPDATE control.acceptance SET run_id = '' WHERE id = 'a2'`,
	}},
	"no-overlapping-lease-intervals": {{
		fmt.Sprintf(`INSERT INTO control.lease_history VALUES ('i','%s',3,'c','2026-01-01 00:20:00+00',NULL,NULL)`, lease.ControllerName),
	}},
	// Epoch 1 writes after epoch 2 at seq 3, with a writer clock two minutes
	// behind: the check must not depend on timestamps.
	"no-stale-owner-emits": {
		{
			`INSERT INTO journal.instance_event (instance_id, stream, seq, at, type, run_id, emit_key, epoch, body) VALUES ('i','main',3,'2026-01-01 00:09:00+00','t','r1','e3',1,'\x00')`,
			`UPDATE journal.instance_seq SET last_seq = 3`,
		},
		// An epoch the controller lease never held.
		{
			`INSERT INTO journal.instance_event (instance_id, stream, seq, at, type, run_id, emit_key, epoch, body) VALUES ('i','main',3,'2026-01-01 00:12:00+00','t','r1','e3',7,'\x00')`,
			`UPDATE journal.instance_seq SET last_seq = 3`,
		},
	},
}

var pendingForm = regexp.MustCompile(`^pending #[0-9]+$`)

func TestRegistryIsWellFormed(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, inv := range All() {
		if inv.Name == "" || seen[inv.Name] {
			t.Fatalf("invariant name %q empty or duplicate", inv.Name)
		}
		seen[inv.Name] = true
		switch {
		case inv.Pending != "" && inv.SQL != "":
			t.Errorf("%s has both SQL and Pending", inv.Name)
		case inv.Pending != "":
			if !pendingForm.MatchString(inv.Pending) {
				t.Errorf("%s pending %q must match %q", inv.Name, inv.Pending, pendingForm)
			}
			if _, ok := injections[inv.Name]; ok {
				t.Errorf("%s is pending but has an injection", inv.Name)
			}
		case inv.SQL == "" || len(inv.Reads) == 0 || inv.CoverageSQL == "":
			t.Errorf("%s has no SQL, Reads or CoverageSQL", inv.Name)
		default:
			if len(injections[inv.Name]) == 0 {
				t.Errorf("%s has no injected-violation self-test", inv.Name)
			}
		}
	}
}

func TestPendingInvariantsAreReportedSkipped(t *testing.T) {
	t.Parallel()
	report, err := CheckSet(t.Context(), nil, []Invariant{{Name: "x", Pending: "pending #1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Checked) != 0 || len(report.Skipped) != 1 || report.Skipped[0].Reason != "pending #1" || !report.OK() {
		t.Fatalf("%+v", report)
	}
}

func TestWriteEvidence(t *testing.T) {
	t.Parallel()
	if err := (Report{Checked: []string{"x"}}).WriteEvidence(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
