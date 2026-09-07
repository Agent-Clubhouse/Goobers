package readservice

import (
	"context"
	"reflect"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestIsolationMandatesStatusUsesConfiguredFloorWithoutSharingSlices(t *testing.T) {
	service, _, _ := fixtureService(t)
	service.sources.Config = &instance.Config{Isolation: &instance.IsolationConfig{Mandates: []instance.IsolationMandate{{Match: instance.IsolationMatch{StageClass: "agentic"}, Restrictions: []instance.RunnerRestriction{instance.RunnerRestrictionTmpEphemeral}}}}}
	first, err := service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"agentic": {"tmp:ephemeral"}}
	if !reflect.DeepEqual(first.IsolationMandates, want) {
		t.Fatalf("floor: %+v", first.IsolationMandates)
	}
	first.IsolationMandates["agentic"][0] = "changed"
	second, err := service.SchedulerStatus(context.Background())
	if err != nil || !reflect.DeepEqual(second.IsolationMandates, want) {
		t.Fatalf("aliased policy: %+v, %v", second, err)
	}
}
