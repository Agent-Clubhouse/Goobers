package scheduling

import (
	"errors"
	"fmt"
	"sync"

	"github.com/goobers/goobers/internal/configgeneration"
)

// Runtime owns generation leases during definition construction and until the
// caller drains dispatched work. Registries and compiled definitions remain
// owned by the composition root, accessed through RuntimeDependencies.
type Runtime struct {
	Generations           *configgeneration.Retainer
	CapacityRejection     string
	RecoveredConfigDigest string
	closeOnce             sync.Once
	closeErr              error
}

// RuntimeDependencies supplies the definition graph without importing command
// types. Bind runs before Build or Recover can acquire generation leases.
type RuntimeDependencies struct {
	NewRetainer       func() (*configgeneration.Retainer, error)
	Bind              func(*configgeneration.Retainer)
	ExplicitRecovery  bool
	RecoverAtCapacity func() bool
	Recover           func() (string, error)
	Build             func() error
	Install           func() error
	CapacityMessage   func(error) string
	Progress          func(string)
}

// Open rolls back generation leases on every construction failure. Failed
// owners have released all leases and must not be reused.
func (owned *Runtime) Open(deps RuntimeDependencies) (err error) {
	defer func() {
		if err != nil {
			_ = owned.Close()
		}
	}()
	owned.Generations, err = deps.NewRetainer()
	if err != nil {
		return err
	}
	deps.Bind(owned.Generations)
	if deps.ExplicitRecovery {
		owned.CapacityRejection = "operator restored a verified retained catalog; live candidate configuration was not admitted"
		owned.RecoveredConfigDigest, err = deps.Recover()
		if err != nil {
			return fmt.Errorf("explicit retained catalog recovery refused: %w", err)
		}
	}
	err = deps.Build()
	if errors.Is(err, configgeneration.ErrCapacity) && deps.RecoverAtCapacity() {
		owned.CapacityRejection = deps.CapacityMessage(err)
		owned.RecoveredConfigDigest, err = deps.Recover()
		if err != nil {
			return fmt.Errorf("%s; retained catalog recovery refused: %w", owned.CapacityRejection, err)
		}
		deps.Progress("candidate rejected at retention capacity; rebuilding verified retained catalog")
		err = deps.Build()
	}
	if err != nil {
		return err
	}
	return deps.Install()
}

// Close releases the owned generation leases exactly once.
func (owned *Runtime) Close() error {
	if owned == nil {
		return nil
	}
	owned.closeOnce.Do(func() {
		if owned.Generations != nil {
			owned.closeErr = owned.Generations.Close()
		}
	})
	return owned.closeErr
}
