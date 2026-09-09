package readservice

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

func TestInstanceRootIdentityReadOnlyAndHistorical(t *testing.T) {
	service, layout := newInventoryService(t, inventoryDefinitions(), nil)
	view, err := service.Instance(context.Background())
	if err != nil || view.RootIdentity == nil || view.RootIdentity.IdentityProblem == "" {
		t.Fatalf("legacy identity: %+v %v", view.RootIdentity, err)
	}
	if _, err := instance.ReadRootIdentity(layout.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection adopted legacy root: %v", err)
	}
	if err := os.WriteFile(layout.ConfigFile(), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := instance.EnsureRootIdentity(context.Background(), layout.Root)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := instance.DecommissionRoot(context.Background(), layout.Root, "migrated", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	view, err = service.Instance(context.Background())
	if err != nil || view.RootIdentity.ID != id || view.RootIdentity.DecommissionedAt == nil || !view.RootIdentity.DecommissionedAt.Equal(marker.At) || view.RootIdentity.DecommissionReason != "migrated" {
		t.Fatalf("historical identity: %+v %v", view.RootIdentity, err)
	}
}
