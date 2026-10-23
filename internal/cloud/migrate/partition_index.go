package migrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SQLExecutor is the subset needed for concurrent partition index creation.
type SQLExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
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
		exists, valid, table, indexedColumns, err := inspectIndex(ctx, executor, partition.Index)
		if err != nil {
			return err
		}
		if exists && !valid {
			if _, err := executor.Exec(ctx, "DROP INDEX CONCURRENTLY "+pgx.Identifier{partition.Index}.Sanitize()); err != nil {
				return fmt.Errorf("drop invalid partition index %s: %w", partition.Index, err)
			}
			exists = false
		}
		if exists && (table != partition.Table || !slices.Equal(indexedColumns, columns)) {
			return fmt.Errorf("partition index %s exists with a different definition", partition.Index)
		}
		statement := fmt.Sprintf(
			"CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s (%s)",
			pgx.Identifier{partition.Index}.Sanitize(),
			pgx.Identifier{partition.Table}.Sanitize(),
			strings.Join(columnNames, ", "),
		)
		if !exists {
			if _, err := executor.Exec(ctx, statement); err != nil {
				return fmt.Errorf("create partition index %s: %w", partition.Index, err)
			}
		}
		var attached bool
		if err := executor.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_inherits
				WHERE inhparent=to_regclass($1) AND inhrelid=to_regclass($2)
			)`,
			parentIndex, partition.Index,
		).Scan(&attached); err != nil {
			return fmt.Errorf("inspect partition index attachment %s: %w", partition.Index, err)
		}
		if !attached {
			if _, err := executor.Exec(ctx, fmt.Sprintf(
				"ALTER INDEX %s ATTACH PARTITION %s",
				pgx.Identifier{parentIndex}.Sanitize(),
				pgx.Identifier{partition.Index}.Sanitize(),
			)); err != nil {
				return fmt.Errorf("attach partition index %s: %w", partition.Index, err)
			}
		}
	}
	return nil
}

func inspectIndex(ctx context.Context, executor SQLExecutor, name string) (bool, bool, string, []string, error) {
	var valid bool
	var table string
	var columns []string
	err := executor.QueryRow(ctx, `
		SELECT idx.indisvalid, indexed.relname,
			ARRAY(
				SELECT attribute.attname
				FROM unnest(idx.indkey) WITH ORDINALITY AS index_key(attnum, position)
				JOIN pg_attribute AS attribute
				  ON attribute.attrelid=idx.indrelid AND attribute.attnum=index_key.attnum
				ORDER BY index_key.position
			)
		FROM pg_index AS idx
		JOIN pg_class AS named ON named.oid=idx.indexrelid
		JOIN pg_namespace AS namespace ON namespace.oid=named.relnamespace
		JOIN pg_class AS indexed ON indexed.oid=idx.indrelid
		WHERE namespace.nspname=current_schema() AND named.relname=$1`,
		name,
	).Scan(&valid, &table, &columns)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, "", nil, nil
	}
	if err != nil {
		return false, false, "", nil, fmt.Errorf("inspect partition index %s: %w", name, err)
	}
	return true, valid, table, columns, nil
}
