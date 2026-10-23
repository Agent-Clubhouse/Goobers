package planes

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDurableTriggerDrainTreatsPruneBudgetExpiryAsBoundedMaintenance(t *testing.T) {
	trigger := NewTriggerService()
	service, err := NewDurableTriggerService(filepath.Join(t.TempDir(), "accepted.db"), trigger, durableBackendStub{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	service.pruneBudget = time.Nanosecond
	if err := service.Drain(t.Context()); err != nil {
		t.Fatalf("expired maintenance budget failed dispatch: %v", err)
	}
}
