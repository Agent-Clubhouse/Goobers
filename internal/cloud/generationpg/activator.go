package generationpg

import (
	"context"
	"errors"
	"sync"

	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/objstore"
	"github.com/goobers/goobers/internal/configgeneration"
)

// Activator publishes and applies one instance's config generations.
type Activator struct {
	metadata   *objstore.Metadata
	instanceID string

	mu      sync.RWMutex
	manager *cloudlease.Manager
	handle  *cloudlease.Handle
}

// NewActivator constructs a generation activator.
func NewActivator(
	metadata *objstore.Metadata,
	instanceID string,
) (*Activator, error) {
	if metadata == nil || instanceID == "" {
		return nil, errors.New("cloud generation activator requires metadata and instance")
	}
	return &Activator{metadata: metadata, instanceID: instanceID}, nil
}

// BindFence installs the current controller lease authority.
func (a *Activator) BindFence(
	manager *cloudlease.Manager,
	handle *cloudlease.Handle,
) {
	a.mu.Lock()
	a.manager, a.handle = manager, handle
	a.mu.Unlock()
}

// Activate verifies, publishes, and atomically applies candidate.
func (a *Activator) Activate(
	ctx context.Context,
	candidate cloudconfig.Candidate,
) error {
	archive, err := configgeneration.Decode(
		candidate.Archive, candidate.Digest)
	if err != nil {
		return err
	}
	if archive.InstanceID != a.instanceID {
		return objstore.ErrGenerationInvalid
	}
	a.mu.RLock()
	manager, handle := a.manager, a.handle
	a.mu.RUnlock()
	return a.metadata.RecordAndApplyGeneration(
		ctx, manager, handle, a.instanceID,
		candidate.Digest, candidate.Archive)
}

var _ cloudconfig.Activator = (*Activator)(nil)
