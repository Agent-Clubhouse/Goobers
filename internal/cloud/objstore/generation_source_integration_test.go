//go:build cloudintegration

package objstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/configgeneration"
)

func TestGenerationSourceAsControllerRole(t *testing.T) {
	_, admin, store := testMetadata(t, "goobers-generation-source-role")
	controller := controllerRolePool(t, admin)
	metadata, err := NewMetadataWithOptions(
		controller, store, MetadataOptions{})
	if err != nil {
		t.Fatal(err)
	}
	archive, digest, err := configgeneration.CaptureForInstance(
		t.Context(),
		filepath.Join("..", "..", "instance", "starter"),
		"instance",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.RecordGeneration(
		t.Context(), "instance", digest, archive); err != nil {
		t.Fatal(err)
	}
	source, err := NewGenerationSource(metadata, "instance", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	generation, err := source.Acquire(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if generation.Set == nil || len(generation.Set.Gaggles) == 0 {
		t.Fatal("retained generation did not load its ConfigSet")
	}
	info, err := os.Stat(generation.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0222 != 0 {
		t.Fatalf("generation directory mode = %o, want read-only", info.Mode().Perm())
	}
	var pins int
	if err := admin.QueryRow(t.Context(), `
		SELECT count(*) FROM control.config_generation_pin
		WHERE instance_id='instance' AND digest=$1`,
		digest,
	).Scan(&pins); err != nil {
		t.Fatal(err)
	}
	if pins != 1 {
		t.Fatalf("read pins = %d, want 1", pins)
	}
	if err := metadata.PruneGeneration(
		t.Context(), "instance", digest); !errors.Is(err, ErrPinned) {
		t.Fatalf("prune while acquired = %v, want ErrPinned", err)
	}
	directory := filepath.Dir(generation.Directory)
	if err := generation.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released generation directory still exists: %v", err)
	}
	if err := admin.QueryRow(t.Context(), `
		SELECT count(*) FROM control.config_generation_pin
		WHERE instance_id='instance' AND digest=$1`,
		digest,
	).Scan(&pins); err != nil {
		t.Fatal(err)
	}
	if pins != 0 {
		t.Fatalf("released read pins = %d", pins)
	}
	first, err := source.Acquire(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.Acquire(t.Context(), digest)
	if err != nil {
		_ = first.Release()
		t.Fatal(err)
	}
	if first.Directory == second.Directory {
		t.Fatal("concurrent acquisitions share a mutable lifetime directory")
	}
	if err := admin.QueryRow(t.Context(), `
		SELECT count(*) FROM control.config_generation_pin
		WHERE instance_id='instance' AND digest=$1`,
		digest,
	).Scan(&pins); err != nil {
		t.Fatal(err)
	}
	if pins != 2 {
		t.Fatalf("concurrent read pins = %d, want 2", pins)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second.Directory); err != nil {
		t.Fatalf("first release invalidated second acquisition: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	if err := source.PinGeneration(
		t.Context(), digest, "child-acceptance"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PruneGeneration(
		t.Context(), "instance", digest); !errors.Is(err, ErrPinned) {
		t.Fatalf("prune with durable child pin = %v, want ErrPinned", err)
	}
	if err := source.UnpinGeneration(
		t.Context(), digest, "child-acceptance"); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSourceClassifiesMissingAndForeign(t *testing.T) {
	metadata, _, _ := testMetadata(t, "goobers-generation-source-errors")
	source, err := NewGenerationSource(metadata, "instance", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	missing := "sha256:" + strings.Repeat("a", 64)
	if _, err := source.Acquire(
		t.Context(), missing); !errors.Is(err, ErrGenerationNotFound) {
		t.Fatalf("missing generation = %v, want ErrGenerationNotFound", err)
	}
	archive, digest, err := configgeneration.CaptureForInstance(
		t.Context(),
		filepath.Join("..", "..", "instance", "starter"),
		"other-instance",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.RecordGeneration(
		t.Context(), "instance", digest, archive); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Acquire(
		t.Context(), digest); !errors.Is(err, ErrGenerationInvalid) {
		t.Fatalf("foreign generation = %v, want ErrGenerationInvalid", err)
	}
}

func TestRecordAndApplyGenerationAsControllerRole(t *testing.T) {
	_, admin, store := testMetadata(t, "goobers-generation-apply-role")
	controller := controllerRolePool(t, admin)
	metadata, err := NewMetadataWithOptions(
		controller, store, MetadataOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := cloudlease.New(cloudlease.Config{
		Pool: controller, InstanceID: "instance",
		Name: cloudlease.ControllerName, Holder: "controller-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	archive, digest, err := configgeneration.CaptureForInstance(
		t.Context(),
		filepath.Join("..", "..", "instance", "starter"),
		"instance",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.RecordAndApplyGeneration(
		t.Context(), manager, handle, "instance", digest, archive,
	); err != nil {
		t.Fatal(err)
	}
	var appliedDigest string
	var generation int64
	if err := admin.QueryRow(t.Context(), `
		SELECT digest,generation FROM control.applied_config
		WHERE instance_id='instance'`,
	).Scan(&appliedDigest, &generation); err != nil {
		t.Fatal(err)
	}
	if appliedDigest != digest || generation != 1 {
		t.Fatalf("applied config = %s/%d, want %s/1",
			appliedDigest, generation, digest)
	}
	if err := metadata.RecordAndApplyGeneration(
		t.Context(), manager, handle, "instance", digest, archive,
	); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(t.Context(), `
		SELECT generation FROM control.applied_config
		WHERE instance_id='instance'`,
	).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 1 {
		t.Fatalf("exact replay advanced generation to %d", generation)
	}
}

func controllerRolePool(
	t *testing.T,
	admin *pgxpool.Pool,
) *pgxpool.Pool {
	t.Helper()
	config := admin.Config().Copy()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
