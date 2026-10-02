package readservice

import (
	"context"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestStageServiceAccountsStatus(t *testing.T) {
	defs := testDefinitions()
	defs.Gaggles = []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}}, {ObjectMeta: metav1.ObjectMeta{Name: "beta"}, Spec: apiv1.GaggleSpec{Isolation: apiv1.GaggleIsolation{ServiceAccount: "default"}}}}
	service, err := NewLocal(LocalSources{Definitions: defs}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(status.StageServiceAccounts, map[string]string{"alpha": "goobers-stage", "beta": "default"}) {
		t.Fatalf("accounts=%v", status.StageServiceAccounts)
	}
}
