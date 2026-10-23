// Package invariants is the SQL invariant checker for HA chaos runs.
//
// Each invariant is a violation query: it selects the offending rows, so an
// empty result means the invariant holds. The queries use only the portable
// SQL subset shared by PostgreSQL and SQLite, so the checker's self-test can
// prove each query detects an injected violation without a cluster. The
// schema-qualified table names (control.*, journal.*) follow the control-plane
// HA design; the columns each query reads are listed in Invariant.Reads.
package invariants

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Queryer is the subset of *sql.DB and *sql.Tx the checker needs.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Invariant is one named violation query.
type Invariant struct {
	Name        string
	Description string
	Reads       []string
	// SQL selects violating rows; no rows means the invariant holds.
	SQL string
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
}

// OK reports whether every checked invariant held.
func (r Report) OK() bool { return len(r.Violations) == 0 }

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
		report.Checked = append(report.Checked, inv.Name)
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
	rs, err := q.QueryContext(ctx, inv.SQL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	cols, err := rs.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]string
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]string, len(cols))
		for i, c := range cols {
			row[c] = fmt.Sprint(vals[i])
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
