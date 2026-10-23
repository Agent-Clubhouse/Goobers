package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SQLExecutor is the subset needed for concurrent partition index creation.
type SQLExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// PartitionIndex names one partition table and its corresponding index.
type PartitionIndex struct {
	Table string
	Index string
}

// CreatePartitionedIndex creates indexes concurrently on each partition and
// then attaches them to an existing parent partitioned index.
func CreatePartitionedIndex(
	ctx context.Context,
	executor SQLExecutor,
	parentIndex string,
	columns []string,
	partitions []PartitionIndex,
) error {
	if executor == nil || parentIndex == "" || len(columns) == 0 || len(partitions) == 0 {
		return errors.New("cloud partition index requires an executor, parent, columns, and partitions")
	}
	columnNames := make([]string, len(columns))
	for i, column := range columns {
		if strings.TrimSpace(column) == "" {
			return errors.New("cloud partition index column is empty")
		}
		columnNames[i] = pgx.Identifier{column}.Sanitize()
	}
	for _, partition := range partitions {
		if partition.Table == "" || partition.Index == "" {
			return errors.New("cloud partition index names must not be empty")
		}
		statement := fmt.Sprintf(
			"CREATE INDEX CONCURRENTLY %s ON %s (%s)",
			pgx.Identifier{partition.Index}.Sanitize(),
			pgx.Identifier{partition.Table}.Sanitize(),
			strings.Join(columnNames, ", "),
		)
		if _, err := executor.Exec(ctx, statement); err != nil {
			return fmt.Errorf("create partition index %s: %w", partition.Index, err)
		}
		if _, err := executor.Exec(ctx, fmt.Sprintf(
			"ALTER INDEX %s ATTACH PARTITION %s",
			pgx.Identifier{parentIndex}.Sanitize(),
			pgx.Identifier{partition.Index}.Sanitize(),
		)); err != nil {
			return fmt.Errorf("attach partition index %s: %w", partition.Index, err)
		}
	}
	return nil
}
