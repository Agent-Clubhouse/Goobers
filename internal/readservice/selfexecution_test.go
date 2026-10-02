package readservice

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestSelfExecutionStatusAndHealth(t *testing.T) {
	service, layout, _ := fixtureService(t)
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{Type: journal.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := &instance.Config{Placement: &instance.PlacementConfig{SelfExecution: "deny"}}
	cfg.StartSelfExecutionAccounting()
	service.sources.Config = cfg
	status, err := service.SchedulerStatus(context.Background())
	if err != nil || status.SelfExecution.Policy != "deny" || !status.SelfExecution.Observed || status.SelfExecution.Placements != 0 {
		t.Fatalf("status: %+v %v", status.SelfExecution, err)
	}
	for _, refused := range []bool{true, false} {
		cfg.ObserveSelfExecution(refused)
		health, err := service.Health(context.Background())
		if err != nil || health.Healthy {
			t.Fatalf("policy alarm health: %+v %v", health, err)
		}
	}
}
