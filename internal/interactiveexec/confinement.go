package interactiveexec

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/sandbox"
)

func (e *Executor) nativeSandbox() (sandbox.Sandbox, error) {
	base := e.config.Sandbox
	if base == nil {
		create := e.newSandbox
		if create == nil {
			create = sandbox.New
		}
		var err error
		base, err = create()
		if err != nil {
			return nil, errors.Join(harness.ErrSandboxUnavailable, err)
		}
	}
	if base == nil || e.config.StateDir == "" {
		return nil, fmt.Errorf("%w: interactive sandbox and private server state are required", harness.ErrSandboxUnavailable)
	}
	state, err := filepath.Abs(e.config.StateDir)
	if err != nil {
		return nil, err
	}
	return sandbox.WithReadDenials(base, []string{state}), nil
}

func chatStatePath(state, workspace string) string {
	return filepath.Join(filepath.Clean(state)+"-chat-state", filepath.Base(workspace))
}
