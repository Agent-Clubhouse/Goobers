package main

import (
	"testing"

	"github.com/goobers/goobers/internal/controlplane/planes"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func durableTriggerServiceForQueue(t *testing.T, queue *triggerqueue.Store, dispatch *daemonTriggerService) *durableTriggerService {
	t.Helper()
	service := &durableTriggerService{queue: queue, dispatch: dispatch}
	core, err := planes.NewDurableTriggerServiceWithQueue(queue, dispatch, service)
	if err != nil {
		t.Fatal(err)
	}
	service.DurableTriggerService = core
	return service
}
