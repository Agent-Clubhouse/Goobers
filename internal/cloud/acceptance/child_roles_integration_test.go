//go:build cloudintegration

package acceptance

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func childRolePool(t *testing.T, owner *pgxpool.Pool, role string) *pgxpool.Pool {
	t.Helper()
	config := owner.Config()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var current string
	if err := pool.QueryRow(t.Context(), "SELECT current_user").Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != role {
		t.Fatalf("current_user=%s, want %s", current, role)
	}
	return pool
}

func TestChildInventoryAllowsBlobProbesAndDeniesAPIMutations(t *testing.T) {
	store, owner := childTestStore(t)
	child, _, err := store.AcceptChild(t.Context(), childRequest("parent", "stage", "call"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Invoke each child probe separately: blob_is_referenced's fail-closed ORs
	// can otherwise short-circuit before reaching a missing column grant.
	var definition string
	if err := owner.QueryRow(t.Context(), "SELECT pg_get_functiondef('control.blob_is_referenced(text)'::regprocedure)").Scan(&definition); err != nil {
		t.Fatal(err)
	}
	probes := regexp.MustCompile(`'control\.(child_[a-z_]+)'[^']*'([a-z_]+)'`).FindAllStringSubmatch(definition, -1)
	if len(probes) == 0 {
		t.Fatal("no child blob-reference probes found")
	}
	for _, probe := range probes {
		var referenced bool
		if err := store.pool.QueryRow(t.Context(), "SELECT control.blob_column_referenced($1,$2,$3)", "control."+probe[1], probe[2], childDigest([]byte("parent-stage"))).Scan(&referenced); err != nil {
			t.Fatalf("%s.%s: %v", probe[1], probe[2], err)
		}
		if probe[1] == "child_lineage" && probe[2] == "actor_digest" && !referenced {
			t.Fatal("live child actor digest is not pinned")
		}
	}
	role := "goobers_api"
	pool := childRolePool(t, owner, role)
	for _, statement := range []string{
		"UPDATE control.child_lineage SET owner_epoch=owner_epoch WHERE child_id=$1",
		"DELETE FROM control.child_lineage WHERE child_id=$1",
		"SELECT actor_digest FROM control.child_lineage WHERE child_id=$1",
	} {
		_, err := pool.Exec(t.Context(), statement, child.ChildID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s permitted %s: %v", role, strings.Fields(statement)[0], err)
		}
	}
	api, err := New(pool, store.instanceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := api.Accept(t.Context(), "ordinary", "actor", []byte("ordinary"), time.Now()); err != nil {
		t.Fatalf("ordinary API retention probe: %v", err)
	}
}
