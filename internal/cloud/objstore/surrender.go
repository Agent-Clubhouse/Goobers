package objstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/dispatcher"
)

// SurrenderPlane adapts immutable PostgreSQL surrender rows to the dispatcher seam.
type SurrenderPlane struct {
	Metadata   *Metadata
	InstanceID string
}

// Describe names the credential-free cloud surrender plane.
func (p SurrenderPlane) Describe() string { return "postgres-surrender" }

// Put records one write-once surrender document.
func (p SurrenderPlane) Put(ctx context.Context, runID, stage string, attempt int, data []byte) error {
	if p.Metadata == nil || p.InstanceID == "" {
		return errors.New("cloud surrender plane is not configured")
	}
	return p.Metadata.PutSurrender(ctx, p.InstanceID, runID, stage, attempt, data)
}

// Get returns one surrender document.
func (p SurrenderPlane) Get(ctx context.Context, runID, stage string, attempt int) ([]byte, error) {
	if p.Metadata == nil || p.InstanceID == "" {
		return nil, errors.New("cloud surrender plane is not configured")
	}
	var data []byte
	err := p.Metadata.pool.QueryRow(ctx, `
		SELECT doc FROM control.surrender
		WHERE instance_id=$1 AND run_id=$2 AND stage=$3 AND attempt=$4`,
		p.InstanceID, runID, stage, attempt,
	).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: run %s stage %s attempt %d", dispatcher.ErrNoSurrender, runID, stage, attempt)
	}
	if err != nil {
		return nil, fmt.Errorf("read cloud surrender: %w", err)
	}
	return data, nil
}

// Has reports whether a surrender document exists.
func (p SurrenderPlane) Has(ctx context.Context, runID, stage string, attempt int) (bool, error) {
	_, err := p.Get(ctx, runID, stage, attempt)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, dispatcher.ErrNoSurrender):
		return false, nil
	default:
		return false, err
	}
}

var _ dispatcher.SurrenderPlane = SurrenderPlane{}
