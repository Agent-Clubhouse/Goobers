// Package migrate owns ordered cloud PostgreSQL schema migrations.
package migrate

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Kind describes whether a migration only expands compatibility or contracts it.
type Kind string

const (
	// Expand adds schema that remains compatible with the previous binary.
	Expand Kind = "expand"
	// Contract removes schema that old binaries may still read.
	Contract Kind = "contract"
)

var migrationName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.(expand|contract)\.sql$`)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migration is one ordered, tagged SQL migration.
type Migration struct {
	Version             int
	Kind                Kind
	BinaryVersion       string
	MinCompatibleBinary string
	Name                string
	SQL                 string
}

// Catalog returns the migrations embedded in this binary.
func Catalog() ([]Migration, error) {
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("cloud migration catalog: %w", err)
	}
	migrations := make([]Migration, 0, len(names))
	for _, name := range names {
		content, err := migrationFiles.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("cloud migration catalog: read %s: %w", name, err)
		}
		migration, err := parseMigration(filepath.Base(name), string(content))
		if err != nil {
			return nil, err
		}
		migrations = append(migrations, migration)
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	if err := ValidateCatalog(migrations); err != nil {
		return nil, err
	}
	return migrations, nil
}

func parseMigration(name, sql string) (Migration, error) {
	match := migrationName.FindStringSubmatch(name)
	if match == nil {
		return Migration{}, fmt.Errorf("cloud migration catalog: %s is not tagged expand or contract", name)
	}
	version, err := strconv.Atoi(match[1])
	if err != nil {
		return Migration{}, fmt.Errorf("cloud migration catalog: parse %s version: %w", name, err)
	}
	metadata := make(map[string]string)
	for _, line := range strings.Split(sql, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "-- goobers:") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "-- goobers:"), " ")
		if ok {
			metadata[key] = strings.TrimSpace(value)
		}
	}
	migration := Migration{
		Version: version, Kind: Kind(match[2]), Name: name, SQL: sql,
		BinaryVersion:       metadata["binary-version"],
		MinCompatibleBinary: metadata["min-compatible-binary"],
	}
	if migration.BinaryVersion == "" || migration.MinCompatibleBinary == "" {
		return Migration{}, fmt.Errorf("cloud migration catalog: %s lacks binary compatibility metadata", name)
	}
	return migration, nil
}

// ValidateCatalog enforces ordering and the expand/contract tag policy.
func ValidateCatalog(migrations []Migration) error {
	if len(migrations) == 0 {
		return errors.New("cloud migration catalog is empty")
	}
	seen := make(map[int]struct{}, len(migrations))
	last := 0
	for _, migration := range migrations {
		if migration.Kind != Expand && migration.Kind != Contract {
			return fmt.Errorf("cloud migration %q has invalid kind %q", migration.Name, migration.Kind)
		}
		if migration.Version <= last {
			return fmt.Errorf("cloud migration %q is not in increasing version order", migration.Name)
		}
		if _, exists := seen[migration.Version]; exists {
			return fmt.Errorf("cloud migration version %d is duplicated", migration.Version)
		}
		if strings.TrimSpace(migration.BinaryVersion) == "" || strings.TrimSpace(migration.MinCompatibleBinary) == "" {
			return fmt.Errorf("cloud migration %q lacks compatibility metadata", migration.Name)
		}
		seen[migration.Version] = struct{}{}
		last = migration.Version
	}
	return nil
}
