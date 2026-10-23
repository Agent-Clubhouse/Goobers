package migrate

import (
	"context"
	"errors"
	"fmt"
	"sort"

	version "github.com/hashicorp/go-version"
)

var (
	// ErrBinaryTooOld means the database has contracted beyond this binary.
	ErrBinaryTooOld = errors.New("cloud database requires a newer binary")
	// ErrSchemaNewer means the database contains an unknown contract migration.
	ErrSchemaNewer = errors.New("cloud database schema is newer than this binary")
)

// Applied records schema compatibility metadata persisted by a migration.
type Applied struct {
	Version             int
	Kind                Kind
	BinaryVersion       string
	MinCompatibleBinary string
}

// CheckCompatibility verifies whether a binary may serve the applied schema.
func CheckCompatibility(binary string, applied []Applied, catalog []Migration) error {
	return checkCompatibility(binary, applied, catalog, false)
}

func checkCompatibility(binary string, applied []Applied, catalog []Migration, allowDevelopment bool) error {
	if binary == "dev" {
		if !allowDevelopment {
			return errors.New("development binary version is not valid for cloud schema compatibility")
		}
		binary = "999999.0.0"
	}
	self, err := version.NewVersion(binary)
	if err != nil {
		return fmt.Errorf("parse binary version %q: %w", binary, err)
	}
	known := make(map[int]struct{}, len(catalog))
	for _, migration := range catalog {
		known[migration.Version] = struct{}{}
	}
	ordered := append([]Applied(nil), applied...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Version < ordered[j].Version })
	for _, migration := range ordered {
		minimum, err := version.NewVersion(migration.MinCompatibleBinary)
		if err != nil {
			return fmt.Errorf("parse migration %d minimum binary: %w", migration.Version, err)
		}
		if self.LessThan(minimum) {
			return fmt.Errorf("%w: migration %d requires %s", ErrBinaryTooOld, migration.Version, minimum)
		}
		if _, ok := known[migration.Version]; !ok && migration.Kind != Expand {
			return fmt.Errorf("%w: unknown %s migration %d", ErrSchemaNewer, migration.Kind, migration.Version)
		}
	}
	return nil
}

// IsIncompatible reports a schema state that should make a serving process
// unready without terminating it.
func IsIncompatible(err error) bool {
	return errors.Is(err, ErrBinaryTooOld) || errors.Is(err, ErrSchemaNewer)
}

// ReadmodelBuilder rebuilds and atomically activates a versioned read model.
type ReadmodelBuilder interface {
	Build(context.Context, string) error
	Flip(context.Context, string, int) error
}

// RebuildReadmodel builds a new version before changing the active pointer.
func RebuildReadmodel(ctx context.Context, builder ReadmodelBuilder, name string, version int) error {
	table := fmt.Sprintf("%s_v%d", name, version)
	if err := builder.Build(ctx, table); err != nil {
		return fmt.Errorf("rebuild read model %s: %w", table, err)
	}
	if err := builder.Flip(ctx, name, version); err != nil {
		return fmt.Errorf("activate read model %s: %w", table, err)
	}
	return nil
}
