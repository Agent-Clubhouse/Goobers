package migrate

import (
	"context"
	"fmt"
	"sort"

	version "github.com/hashicorp/go-version"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StatusReport is the read-only cloud schema state shown to operators.
type StatusReport struct {
	Binary                  string    `json:"binary"`
	Compatible              bool      `json:"compatible"`
	CompatibilityError      string    `json:"compatibilityError,omitempty"`
	MinimumCompatibleBinary string    `json:"minimumCompatibleBinary,omitempty"`
	Applied                 []Applied `json:"applied"`
	PendingVersions         []int     `json:"pendingVersions"`
}

// Status reads applied migrations and evaluates binary compatibility.
func Status(ctx context.Context, pool *pgxpool.Pool, binary string) (StatusReport, error) {
	applied, err := AppliedMigrations(ctx, pool)
	if err != nil {
		return StatusReport{}, fmt.Errorf("read cloud migration status: %w", err)
	}
	catalog, err := Catalog()
	if err != nil {
		return StatusReport{}, err
	}
	report := StatusReport{
		Binary: binary, Compatible: true, Applied: applied,
		MinimumCompatibleBinary: minimumCompatibleBinary(applied),
	}
	if report.Applied == nil {
		report.Applied = []Applied{}
	}
	if err := CheckCompatibility(binary, applied, catalog); err != nil {
		report.Compatible = false
		report.CompatibilityError = err.Error()
	}
	seen := make(map[int]struct{}, len(applied))
	for _, migration := range applied {
		seen[migration.Version] = struct{}{}
	}
	for _, migration := range catalog {
		if _, ok := seen[migration.Version]; !ok {
			report.PendingVersions = append(report.PendingVersions, migration.Version)
		}
	}
	if report.PendingVersions == nil {
		report.PendingVersions = []int{}
	}
	return report, nil
}

func minimumCompatibleBinary(applied []Applied) string {
	type parsed struct {
		raw     string
		version *version.Version
	}
	values := make([]parsed, 0, len(applied))
	for _, migration := range applied {
		value, err := version.NewVersion(migration.MinCompatibleBinary)
		if err == nil {
			values = append(values, parsed{raw: migration.MinCompatibleBinary, version: value})
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i].version.GreaterThan(values[j].version) })
	if len(values) == 0 {
		return ""
	}
	return values[0].raw
}
