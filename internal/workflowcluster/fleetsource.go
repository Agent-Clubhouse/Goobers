package workflowcluster

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/goobers/goobers/internal/fleet"
	"github.com/goobers/goobers/internal/version"
)

const (
	// FleetFeatureStructural serves configured workflow structure.
	FleetFeatureStructural = "structural"
	// FleetFeatureBehavior serves retained run behavior.
	FleetFeatureBehavior = "behavior"
)

var (
	// ErrFleetNotAssociated means the instance has no fleet association.
	ErrFleetNotAssociated = errors.New("workflowcluster: instance is not fleet-associated")
	// ErrFleetAssociationRevoked means the fleet association is revoked.
	ErrFleetAssociationRevoked = errors.New("workflowcluster: fleet association is revoked")
	// ErrFleetProjectionNotGranted means the association does not grant the
	// workflow-feature projection.
	ErrFleetProjectionNotGranted = errors.New("workflowcluster: labs.workflowFeatures projection is not granted")
)

// AssociationReader reads the non-secret fleet association metadata.
type AssociationReader interface {
	LoadAssociation(string) (fleet.Association, error)
}

// FleetSnapshotSource builds scope-gated snapshots for the instance API.
type FleetSnapshotSource struct {
	root              string
	associations      AssociationReader
	now               func() time.Time
	goobersVersion    func() string
	configuredPoints  func(string) ([]Point, error)
	runBehaviorPoints func(string) ([]Point, error)
}

// NewFleetSnapshotSource creates an API snapshot source for root.
func NewFleetSnapshotSource(root string, associations AssociationReader) (*FleetSnapshotSource, error) {
	if associations == nil {
		return nil, errors.New("workflowcluster: fleet association reader is required")
	}
	return &FleetSnapshotSource{
		root:           root,
		associations:   associations,
		now:            time.Now,
		goobersVersion: func() string { return version.Get().Version },
		configuredPoints: func(root string) ([]Point, error) {
			points, _, err := configuredPoints(root)
			return points, err
		},
		runBehaviorPoints: func(root string) ([]Point, error) {
			_, points, err := behaviorPoints(root, BehaviorScanOptions{}, cliMinSamples)
			return points, err
		},
	}, nil
}

// WorkflowFeaturesAvailability verifies the durable projection grant.
func (s *FleetSnapshotSource) WorkflowFeaturesAvailability(ctx context.Context) error {
	_, err := s.authorizedAssociation(ctx)
	return err
}

// WorkflowFeatures builds the requested privacy-safe fleet snapshot.
func (s *FleetSnapshotSource) WorkflowFeatures(ctx context.Context, features string) (FleetSnapshot, error) {
	association, err := s.authorizedAssociation(ctx)
	if err != nil {
		return FleetSnapshot{}, err
	}
	var featureVersion string
	var points []Point
	switch features {
	case FleetFeatureStructural:
		featureVersion = StructuralFeatureVersion
		points, err = s.configuredPoints(s.root)
	case FleetFeatureBehavior:
		featureVersion = BehaviorFeatureVersion
		points, err = s.runBehaviorPoints(s.root)
	default:
		return FleetSnapshot{}, fmt.Errorf("workflowcluster: unsupported fleet feature set %q", features)
	}
	if err != nil {
		return FleetSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return FleetSnapshot{}, err
	}
	return NewFleetSnapshot(
		featureVersion,
		association.InstanceID,
		s.goobersVersion(),
		association.FleetID,
		points,
		s.now(),
	)
}

func (s *FleetSnapshotSource) authorizedAssociation(ctx context.Context) (fleet.Association, error) {
	if err := ctx.Err(); err != nil {
		return fleet.Association{}, err
	}
	association, err := s.associations.LoadAssociation(s.root)
	if errors.Is(err, fleet.ErrNotAssociated) {
		return fleet.Association{}, ErrFleetNotAssociated
	}
	if err != nil {
		return fleet.Association{}, fmt.Errorf("workflowcluster: read fleet association: %w", err)
	}
	if association.Revoked {
		return fleet.Association{}, ErrFleetAssociationRevoked
	}
	if !slices.Contains(association.ActiveProjections(), fleet.ProjectionLabsWorkflowFeatures) {
		return fleet.Association{}, ErrFleetProjectionNotGranted
	}
	return association, nil
}
