package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/safepath"
)

// PrivateInput is stage input the agent must read but no reader of its run
// may see, such as a Labs Fluid UI request's text (#8425). It reaches the
// agent as a workspace file and is never journaled: every Sensitive value is
// registered with the run's secret registrar first, so the transcript,
// artifacts and events the stage produces carry the redaction placeholder in
// its place.
type PrivateInput struct {
	// Name is the file name under .goobers/private/ and the label the prompt
	// lists it under.
	Name string
	Data []byte
	// Sensitive lists the exact values to redact from everything the run
	// records. Values shorter than the registry's minimum are not redacted, so
	// a source must not hand over a value it cannot protect.
	Sensitive [][]byte
}

// PrivateInputSource resolves the private inputs of one invocation. It
// returns nothing for an invocation that has none, and an error when an
// invocation that needs one cannot be served.
type PrivateInputSource func(ctx context.Context, env apiv1.InvocationEnvelope) ([]PrivateInput, error)

const privateInputDir = ".goobers/private"

// WithPrivateInputs hands each invocation the private inputs source resolves.
// registrar must be the run's own secret registrar, the registry the run's
// journal scrubs with; without one every private input is refused.
func WithPrivateInputs(source PrivateInputSource, registrar credentials.SecretRegistrar) Option {
	return func(e *Executor) {
		e.privateInputs = source
		e.privateRegistrar = registrar
	}
}

// materializePrivateInputs registers every sensitive value before writing the
// inputs into the workspace, so nothing this stage records can carry them.
// The returned cleanup removes the files once the session ends, so a retained
// workspace does not keep them.
func (e *Executor) materializePrivateInputs(ctx context.Context, env apiv1.InvocationEnvelope) (map[string]string, func(), error) {
	noop := func() {}
	if e.privateInputs == nil {
		return nil, noop, nil
	}
	inputs, err := e.privateInputs(ctx, env)
	if err != nil {
		return nil, noop, fmt.Errorf("harness: resolve private inputs: %w", err)
	}
	if len(inputs) == 0 {
		return nil, noop, nil
	}
	if e.privateRegistrar == nil {
		return nil, noop, errors.New("harness: private inputs need the run's secret registrar")
	}
	for _, input := range inputs {
		for _, value := range input.Sensitive {
			e.privateRegistrar.Register(value)
		}
	}
	paths := make(map[string]string, len(inputs))
	written := make([]string, 0, len(inputs))
	cleanup := func() {
		for _, full := range written {
			_ = os.Remove(full)
		}
	}
	for i, input := range inputs {
		relPath := filepath.Join(privateInputDir, safeContextFilename(i, input.Name))
		full, err := safepath.Resolve(env.Workspace, relPath, true)
		if err == nil {
			err = os.WriteFile(full, input.Data, 0o600)
		}
		if err != nil {
			cleanup()
			return nil, noop, fmt.Errorf("harness: write private input %q: %w", input.Name, err)
		}
		written = append(written, full)
		paths[input.Name] = relPath
	}
	return paths, cleanup, nil
}

func privateInputNames(paths map[string]string) []string {
	names := make([]string, 0, len(paths))
	for name := range paths {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
