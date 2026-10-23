// Package schema owns cloud database write-path and grant policy.
package schema

import (
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FenceKind identifies the invariant that prevents stale or duplicate writes.
type FenceKind string

const (
	// FenceEpoch requires a current lease epoch.
	FenceEpoch FenceKind = "epoch"
	// FenceOwnerToken requires a row-scoped owner token.
	FenceOwnerToken FenceKind = "owner_token"
	// FenceOwnerEpoch requires the row's owning epoch.
	FenceOwnerEpoch FenceKind = "owner_epoch"
	// FenceUniqueKey relies on database uniqueness for idempotency.
	FenceUniqueKey FenceKind = "unique key"
	// FenceNoneByDesign documents a write path that is its own authority.
	FenceNoneByDesign FenceKind = "none-by-design"
)

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

//go:embed writepaths.yaml
var inventoryYAML []byte

// Inventory is the checked-in table write policy.
type Inventory struct {
	Tables []Table `yaml:"tables"`
}

// Table describes one cloud table and its authorized writers.
type Table struct {
	Schema      string    `yaml:"schema"`
	Name        string    `yaml:"table"`
	Provisioned bool      `yaml:"provisioned"`
	Fence       FenceKind `yaml:"fence"`
	Rationale   string    `yaml:"rationale"`
	Readers     []Reader  `yaml:"readers"`
	Writers     []Writer  `yaml:"writers"`
}

// Reader describes column-level read privileges for one database role.
type Reader struct {
	Role    string   `yaml:"role"`
	Columns []string `yaml:"columns"`
}

// Writer describes column-level write privileges for one database role.
type Writer struct {
	Role   string   `yaml:"role"`
	Insert []string `yaml:"insert"`
	Update []string `yaml:"update"`
	Delete bool     `yaml:"delete"`
}

// LoadWritePaths parses and validates the embedded inventory.
func LoadWritePaths() (Inventory, error) {
	var inventory Inventory
	if err := yaml.Unmarshal(inventoryYAML, &inventory); err != nil {
		return Inventory{}, fmt.Errorf("parse cloud write-path inventory: %w", err)
	}
	if err := inventory.Validate(); err != nil {
		return Inventory{}, err
	}
	return inventory, nil
}

// Validate checks that every row is explicit, unique, and grant-safe.
func (inventory Inventory) Validate() error {
	if len(inventory.Tables) == 0 {
		return errors.New("cloud write-path inventory is empty")
	}
	seen := make(map[string]struct{}, len(inventory.Tables))
	for _, table := range inventory.Tables {
		key := table.Schema + "." + table.Name
		if !identifier.MatchString(table.Schema) || !identifier.MatchString(table.Name) {
			return fmt.Errorf("cloud write-path inventory has invalid table %q", key)
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("cloud write-path inventory duplicates %s", key)
		}
		seen[key] = struct{}{}
		if strings.TrimSpace(table.Rationale) == "" {
			return fmt.Errorf("cloud write-path inventory lacks rationale for %s", key)
		}
		switch table.Fence {
		case FenceEpoch, FenceOwnerToken, FenceOwnerEpoch, FenceUniqueKey, FenceNoneByDesign:
		default:
			return fmt.Errorf("cloud write-path inventory has invalid fence %q for %s", table.Fence, key)
		}
		for _, reader := range table.Readers {
			if !identifier.MatchString(reader.Role) || len(reader.Columns) == 0 {
				return fmt.Errorf("cloud write-path inventory has invalid reader %q on %s", reader.Role, key)
			}
			for _, column := range reader.Columns {
				if !identifier.MatchString(column) {
					return fmt.Errorf("cloud write-path inventory has invalid read column %q on %s", column, key)
				}
			}
		}
		for _, writer := range table.Writers {
			if !identifier.MatchString(writer.Role) {
				return fmt.Errorf("cloud write-path inventory has invalid role %q", writer.Role)
			}
			if len(writer.Insert) == 0 && len(writer.Update) == 0 && !writer.Delete {
				return fmt.Errorf("cloud write-path inventory has empty writer %s on %s", writer.Role, key)
			}
			for _, column := range append(append([]string(nil), writer.Insert...), writer.Update...) {
				if !identifier.MatchString(column) {
					return fmt.Errorf("cloud write-path inventory has invalid column %q on %s", column, key)
				}
			}
		}
	}
	return nil
}

// GrantMigrationSQL deterministically renders the owner-applied role migration.
func GrantMigrationSQL(inventory Inventory) string {
	var output strings.Builder
	output.WriteString("-- goobers:binary-version 0.7.0\n")
	output.WriteString("-- goobers:min-compatible-binary 0.6.0\n\n")
	for _, role := range []string{"goobers_api", "goobers_controller"} {
		fmt.Fprintf(&output, "DO $$ BEGIN CREATE ROLE %s NOLOGIN; EXCEPTION WHEN duplicate_object THEN NULL; END $$;\n", role)
	}
	output.WriteString("\nGRANT USAGE ON SCHEMA control TO goobers_api, goobers_controller;\n\n")
	tables := append([]Table(nil), inventory.Tables...)
	sort.Slice(tables, func(i, j int) bool {
		return tables[i].Schema+"."+tables[i].Name < tables[j].Schema+"."+tables[j].Name
	})
	for _, table := range tables {
		if !table.Provisioned {
			continue
		}
		qualified := table.Schema + "." + table.Name
		fmt.Fprintf(&output, "REVOKE SELECT, INSERT, UPDATE, DELETE ON %s FROM goobers_api, goobers_controller;\n", qualified)
		for _, reader := range table.Readers {
			fmt.Fprintf(&output, "GRANT SELECT (%s) ON %s TO %s;\n", strings.Join(reader.Columns, ", "), qualified, reader.Role)
		}
		for _, writer := range table.Writers {
			if len(writer.Insert) > 0 {
				fmt.Fprintf(&output, "GRANT INSERT (%s) ON %s TO %s;\n", strings.Join(writer.Insert, ", "), qualified, writer.Role)
			}
			if len(writer.Update) > 0 {
				fmt.Fprintf(&output, "GRANT UPDATE (%s) ON %s TO %s;\n", strings.Join(writer.Update, ", "), qualified, writer.Role)
			}
			if writer.Delete {
				fmt.Fprintf(&output, "GRANT DELETE ON %s TO %s;\n", qualified, writer.Role)
			}
		}
		output.WriteByte('\n')
	}
	return strings.TrimRight(output.String(), "\n") + "\n"
}

// WriterFor returns a role's policy for one table.
func (inventory Inventory) WriterFor(schema, table, role string) (Writer, bool) {
	for _, candidate := range inventory.Tables {
		if candidate.Schema != schema || candidate.Name != table {
			continue
		}
		for _, writer := range candidate.Writers {
			if writer.Role == role {
				return writer, true
			}
		}
	}
	return Writer{}, false
}
