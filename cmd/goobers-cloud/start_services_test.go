package main

import (
	"testing"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/engine"
)

func TestCloudStarterFactoryRefusesBeforeControllerBindsLease(t *testing.T) {
	services := newDatabaseServices(nil, &acceptance.Store{}, nil)
	if services.BindControllerFence == nil || services.RecoverStarts == nil || services.StartStatusPage == nil {
		t.Fatal("cloud start custody hooks missing")
	}
	starter := services.NewStarter(nil, "cloud")
	_, err := starter.Start(t.Context(), engine.RunInput{AcceptanceID: "trigger-invented", RunID: "invented", OwnerEpoch: 7, StarterID: "invented"})
	if err == nil {
		t.Fatal("unbound cloud starter authorized fabricated custody")
	}
}
