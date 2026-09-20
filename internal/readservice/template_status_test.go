package readservice

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/gaggletemplate"
	"github.com/goobers/goobers/internal/instance"
)

func TestGaggleTemplateInventoryIsOptionalAndOffline(t *testing.T) {
	root := t.TempDir()
	service, err := NewLocal(LocalSources{Layout: instance.NewLayout(root), Definitions: inventoryDefinitions()}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.Gaggles(context.Background(), PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.Template != nil {
			t.Fatal("legacy gaggle gained template state")
		}
	}
	metadata := filepath.Join(root, "config", "gaggles", "alpha", ".template")
	if err := os.MkdirAll(metadata, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metadata, "lock.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	if err := gaggletemplate.WriteStatus(root, "alpha", gaggletemplate.Status{
		State: "conflicts", Installed: "accepted", Candidate: "candidate", CheckedAt: now, LastSuccess: now,
		Conflicts: []string{"workflow.yaml"}, PendingBackprop: true,
	}); err != nil {
		t.Fatal(err)
	}
	page, err = service.Gaggles(context.Background(), PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Items[0].Template == nil || page.Items[0].Template.State != "conflicts" || page.Items[1].Template != nil {
		t.Fatalf("per-gaggle status missing or leaked: %+v", page.Items)
	}
}
