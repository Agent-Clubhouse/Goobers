// Package invariants is the SQL invariant checker for HA chaos runs.
//
// Each invariant is a PostgreSQL violation query: it selects the offending
// rows, so an empty result means the invariant holds. The queries read the
// shipped control.* and journal.* schemas; the self-test (cloudpg tag) applies
// the real migrations to PostgreSQL 16 and proves each query detects an
// injected violation. An invariant whose source of truth has no table yet is
// declared with a Pending reference and reported as skipped, never as held.
package invariants

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jackc/pgx/v5"
)

// Queryer is the subset of *pgxpool.Pool, *pgx.Conn and pgx.Tx the checker needs.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Invariant is one named violation query.
type Invariant struct {
	Name        string
	Description string
	Reads       []string
	// SQL selects violating rows; no rows means the invariant holds.
	SQL string
	// Args are bound to $1... in SQL.
	Args []any
	// CoverageSQL returns one count: the rows SQL examines. A zero count means
	// the checker looked at nothing (empty or wrong database), which is not a pass.
	CoverageSQL string
	// Pending is "pending #<PR>" when no schema exists to check yet; the
	// invariant is then reported as skipped.
	Pending string
}

// Skip is an invariant that could not be checked yet.
type Skip struct {
	Invariant string `json:"invariant"`
	Reason    string `json:"reason"`
}

// Violation is one invariant with the rows that violated it.
type Violation struct {
	Invariant string              `json:"invariant"`
	Rows      []map[string]string `json:"rows"`
}

// Report is the outcome of one Check.
type Report struct {
	Checked    []string    `json:"checked"`
	Violations []Violation `json:"violations"`
	// Uncovered names invariants whose coverage count was zero.
	Uncovered []string `json:"uncovered,omitempty"`
	Skipped   []Skip   `json:"skipped,omitempty"`
}

// OK reports whether every checked invariant held over a non-empty data set.
func (r Report) OK() bool { return len(r.Violations) == 0 && len(r.Uncovered) == 0 }

// All returns the invariant set in a stable order.
func All() []Invariant {
	out := make([]Invariant, len(registry))
	copy(out, registry)
	return out
}

// Check runs every invariant against q. A query error is returned, never
// folded into the report, so an unreadable schema cannot pass silently.
func Check(ctx context.Context, q Queryer) (Report, error) {
	return CheckSet(ctx, q, registry)
}

// CheckSet runs the given invariants.
func CheckSet(ctx context.Context, q Queryer, set []Invariant) (Report, error) {
	var report Report
	for _, inv := range set {
		if inv.Pending != "" {
			report.Skipped = append(report.Skipped, Skip{Invariant: inv.Name, Reason: inv.Pending})
			continue
		}
		report.Checked = append(report.Checked, inv.Name)
		var covered int64
		rs, err := q.Query(ctx, inv.CoverageSQL)
		if err != nil {
			return report, fmt.Errorf("invariant %s coverage: %w", inv.Name, err)
		}
		if rs.Next() {
			err = rs.Scan(&covered)
		}
		rs.Close()
		if err == nil {
			err = rs.Err()
		}
		if err != nil {
			return report, fmt.Errorf("invariant %s coverage: %w", inv.Name, err)
		}
		if covered == 0 {
			report.Uncovered = append(report.Uncovered, inv.Name)
		}
		rows, err := collect(ctx, q, inv)
		if err != nil {
			return report, fmt.Errorf("invariant %s: %w", inv.Name, err)
		}
		if len(rows) > 0 {
			report.Violations = append(report.Violations, Violation{Invariant: inv.Name, Rows: rows})
		}
	}
	return report, nil
}

func collect(ctx context.Context, q Queryer, inv Invariant) ([]map[string]string, error) {
	rs, err := q.Query(ctx, inv.SQL, inv.Args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	cols := rs.FieldDescriptions()
	var out []map[string]string
	for rs.Next() {
		vals, err := rs.Values()
		if err != nil {
			return nil, err
		}
		row := make(map[string]string, len(cols))
		for i, c := range cols {
			row[c.Name] = fmt.Sprint(vals[i])
		}
		out = append(out, row)
	}
	return out, rs.Err()
}

// WriteEvidence writes the report as invariants.json under dir.
func (r Report) WriteEvidence(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	sort.Strings(r.Checked)
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "invariants.json"), data, 0o644)
}
