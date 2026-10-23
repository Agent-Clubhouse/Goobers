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
	// ErrSchemaNewer means the database contains a migration unknown to this binary.
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
		if _, ok := known[migration.Version]; !ok {
			return fmt.Errorf("%w: migration %d", ErrSchemaNewer, migration.Version)
		}
		minimum, err := version.NewVersion(migration.MinCompatibleBinary)
		if err != nil {
			return fmt.Errorf("parse migration %d minimum binary: %w", migration.Version, err)
		}
		if self.LessThan(minimum) {
			return fmt.Errorf("%w: migration %d requires %s", ErrBinaryTooOld, migration.Version, minimum)
		}
	}
	return nil
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
