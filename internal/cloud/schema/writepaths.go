// Package schema owns cloud database write-path and grant policy.
package schema

import (
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"slices"
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

// Inventory is the checked-in table and sequence privilege policy.
type Inventory struct {
	Tables    []Table    `yaml:"tables"`
	Sequences []Sequence `yaml:"sequences"`
	Functions []Function `yaml:"functions"`
}

// Function describes one cloud function (identified by its identity
// arguments) and the roles that may execute it. A SECURITY DEFINER function
// runs with its owner's rights, so it must be listed here to exist at all.
type Function struct {
	Schema          string   `yaml:"schema"`
	Name            string   `yaml:"function"`
	Arguments       string   `yaml:"arguments"`
	Rationale       string   `yaml:"rationale"`
	SecurityDefiner bool     `yaml:"security_definer"`
	Execute         []string `yaml:"execute"`
}

// Signature is the function's identity, as pg_get_function_identity_arguments
// renders it.
func (f Function) Signature() string { return f.Schema + "." + f.Name + "(" + f.Arguments + ")" }

// Sequence describes one cloud sequence and the roles that may draw from it
// (USAGE: nextval and currval).
type Sequence struct {
	Schema      string   `yaml:"schema"`
	Name        string   `yaml:"sequence"`
	Provisioned bool     `yaml:"provisioned"`
	Rationale   string   `yaml:"rationale"`
	Usage       []string `yaml:"usage"`
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
			if !slices.Contains(RuntimeRoles, reader.Role) || len(reader.Columns) == 0 {
				return fmt.Errorf("cloud write-path inventory has invalid reader %q on %s", reader.Role, key)
			}
			for _, column := range reader.Columns {
				if !identifier.MatchString(column) {
					return fmt.Errorf("cloud write-path inventory has invalid read column %q on %s", column, key)
				}
			}
		}
		for _, writer := range table.Writers {
			if !slices.Contains(RuntimeRoles, writer.Role) {
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
	if err := inventory.validateSequences(seen); err != nil {
		return err
	}
	return inventory.validateFunctions()
}

func (inventory Inventory) validateFunctions() error {
	seen := make(map[string]struct{}, len(inventory.Functions))
	for _, function := range inventory.Functions {
		key := function.Signature()
		if !identifier.MatchString(function.Schema) || !identifier.MatchString(function.Name) {
			return fmt.Errorf("cloud write-path inventory has invalid function %q", key)
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("cloud write-path inventory duplicates function %s", key)
		}
		seen[key] = struct{}{}
		if strings.TrimSpace(function.Rationale) == "" {
			return fmt.Errorf("cloud write-path inventory lacks rationale for function %s", key)
		}
		for _, role := range function.Execute {
			if !slices.Contains(RuntimeRoles, role) {
				return fmt.Errorf("cloud write-path inventory has invalid function role %q on %s", role, key)
			}
		}
	}
	return nil
}

func (inventory Inventory) validateSequences(seen map[string]struct{}) error {
	for _, sequence := range inventory.Sequences {
		key := sequence.Schema + "." + sequence.Name
		if !identifier.MatchString(sequence.Schema) || !identifier.MatchString(sequence.Name) {
			return fmt.Errorf("cloud write-path inventory has invalid sequence %q", key)
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("cloud write-path inventory duplicates %s", key)
		}
		seen[key] = struct{}{}
		if strings.TrimSpace(sequence.Rationale) == "" {
			return fmt.Errorf("cloud write-path inventory lacks rationale for sequence %s", key)
		}
		for _, role := range sequence.Usage {
			if !slices.Contains(RuntimeRoles, role) {
				return fmt.Errorf("cloud write-path inventory has invalid sequence role %q on %s", role, key)
			}
		}
	}
	return nil
}

func (inventory Inventory) tableIndex() map[string]Table {
	index := make(map[string]Table, len(inventory.Tables))
	for _, table := range inventory.Tables {
		index[table.Schema+"."+table.Name] = table
	}
	return index
}

func (inventory Inventory) sequenceIndex() map[string]Sequence {
	index := make(map[string]Sequence, len(inventory.Sequences))
	for _, sequence := range inventory.Sequences {
		index[sequence.Schema+"."+sequence.Name] = sequence
	}
	return index
}

// schemas lists every schema that holds a provisioned inventory object.
func (inventory Inventory) schemas() []string {
	var schemas []string
	for _, table := range inventory.Tables {
		if table.Provisioned && !slices.Contains(schemas, table.Schema) {
			schemas = append(schemas, table.Schema)
		}
	}
	for _, sequence := range inventory.Sequences {
		if sequence.Provisioned && !slices.Contains(schemas, sequence.Schema) {
			schemas = append(schemas, sequence.Schema)
		}
	}
	return schemas
}

// schemaUsers lists, sorted, the roles granted anything on a provisioned
// object in schema; only they get USAGE on it.
func (inventory Inventory) schemaUsers(schema string) []string {
	var roles []string
	add := func(role string) {
		if !slices.Contains(roles, role) {
			roles = append(roles, role)
		}
	}
	for _, table := range inventory.Tables {
		if table.Schema != schema || !table.Provisioned {
			continue
		}
		for _, reader := range table.Readers {
			add(reader.Role)
		}
		for _, writer := range table.Writers {
			add(writer.Role)
		}
	}
	for _, sequence := range inventory.Sequences {
		if sequence.Schema == schema && sequence.Provisioned {
			for _, role := range sequence.Usage {
				add(role)
			}
		}
	}
	slices.Sort(roles)
	return roles
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
