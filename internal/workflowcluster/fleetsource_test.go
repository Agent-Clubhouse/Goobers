package workflowcluster

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/fleet"
)

type associationReaderFunc func(string) (fleet.Association, error)

func (f associationReaderFunc) LoadAssociation(root string) (fleet.Association, error) {
	return f(root)
}

func TestFleetSnapshotSourceRequiresActiveProjection(t *testing.T) {
	tests := []struct {
		name        string
		association fleet.Association
		loadErr     error
		want        error
	}{
		{name: "not associated", loadErr: fleet.ErrNotAssociated, want: ErrFleetNotAssociated},
		{name: "revoked", association: fleet.Association{Revoked: true, Projections: []string{fleet.ProjectionLabsWorkflowFeatures}}, want: ErrFleetAssociationRevoked},
		{name: "projection omitted", association: fleet.Association{}, want: ErrFleetProjectionNotGranted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source, err := NewFleetSnapshotSource("root", associationReaderFunc(func(string) (fleet.Association, error) {
				return test.association, test.loadErr
			}))
			if err != nil {
				t.Fatal(err)
			}
			if err := source.WorkflowFeaturesAvailability(context.Background()); !errors.Is(err, test.want) {
				t.Fatalf("availability error = %v, want %v", err, test.want)
			}
			if _, err := source.WorkflowFeatures(context.Background(), FleetFeatureStructural); !errors.Is(err, test.want) {
				t.Fatalf("snapshot error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestFleetSnapshotSourceBuildsSelectedFeatureSnapshot(t *testing.T) {
	association := fleet.Association{
		InstanceID: "instance-1", FleetID: "fleet-1",
		Projections: []string{fleet.ProjectionLabsWorkflowFeatures},
	}
	source, err := NewFleetSnapshotSource("root", associationReaderFunc(func(string) (fleet.Association, error) {
		return association, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	source.now = func() time.Time { return fleetT0 }
	source.goobersVersion = func() string { return "test-version" }
	structural := []Point{{ID: "secret-gaggle/secret-workflow", Numeric: map[string]float64{"nodes": 1}}}
	behavior := []Point{{ID: "secret-behavior", Numeric: map[string]float64{"visits": 2}}}
	source.configuredPoints = func(root string) ([]Point, error) {
		if root != "root" {
			t.Fatalf("root = %q", root)
		}
		return structural, nil
	}
	source.runBehaviorPoints = func(string) ([]Point, error) { return behavior, nil }

	for _, test := range []struct {
		features string
		version  string
		points   []Point
	}{
		{FleetFeatureStructural, StructuralFeatureVersion, structural},
		{FleetFeatureBehavior, BehaviorFeatureVersion, behavior},
	} {
		snapshot, err := source.WorkflowFeatures(context.Background(), test.features)
		if err != nil {
			t.Fatal(err)
		}
		want, err := NewFleetSnapshot(test.version, "instance-1", "test-version", "fleet-1", test.points, fleetT0)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(snapshot, want) {
			t.Fatalf("%s snapshot = %+v, want %+v", test.features, snapshot, want)
		}
	}
	if _, err := source.WorkflowFeatures(context.Background(), "unknown"); err == nil {
		t.Fatal("unknown features succeeded")
	}
}
