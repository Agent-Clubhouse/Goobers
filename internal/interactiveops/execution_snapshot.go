package interactiveops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/goobers/goobers/internal/instance"
)

func executionRoot(dir, id string) string { return filepath.Join(dir, "executions", id, "instance") }
func actionExecutionRoot(dir string, a *Action) (string, error) {
	if a.ExecutionRoot == "" {
		return filepath.Join(dir, "instance"), nil
	}
	if a.ExecutionRoot != executionRoot(dir, a.ID) {
		return "", errors.New("native execution root does not belong to this action")
	}
	return a.ExecutionRoot, nil
}
func scopeRuns(dir string) ([]Run, error) {
	actions, err := allActions(dir)
	if err != nil {
		return nil, err
	}
	roots := map[string]bool{filepath.Join(dir, "instance"): true}
	for i := range actions {
		if actions[i].ExecutionRoot != "" {
			root, err := actionExecutionRoot(dir, &actions[i])
			if err != nil {
				return nil, err
			}
			roots[root] = true
		}
	}
	out := []Run{}
	for root := range roots {
		runs, err := readRuns(root)
		if err != nil {
			return nil, err
		}
		out = append(out, runs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}

// prepareRunSnapshot executes once while the scope/admission worker is held,
// before the durable dispatch marker. The host callback atomically verifies an
// idle parent and captures its repository; child callbacks may return the
// already-private child snapshot. Configuration remains isolated per action.
func (s *Service) prepareRunSnapshot(ctx context.Context, scope Scope, dir string, a *Action) error {
	if s.opts.RunRepositoryForScope == nil {
		return nil
	} // legacy caller's private scope contract
	if a.ExecutionRoot != "" {
		return errors.New("execution snapshot already prepared; never recapture an existing action")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	source, err := s.repository(scope, dir)
	if err != nil {
		return err
	}
	view, _, err := s.scoped(scope)
	if err != nil {
		return err
	}
	set, err := loadNativeConfig(filepath.Join(dir, "instance", "config"))
	if err != nil {
		return err
	}
	var definition WorkflowValidation
	for _, wf := range set.Workflows {
		if wf.Name == a.Request.Workflow && wf.Spec.Gaggle == a.Request.Gaggle {
			data, err := marshalConfig(wf)
			if err != nil {
				return err
			}
			definition = view.validateWorkflow(string(data), source)
			break
		}
	}
	if !definition.Valid {
		return validationError(definition)
	}
	captured, err := s.opts.RunRepositoryForScope(ctx, scope, a.OperationID)
	if err != nil {
		return fmt.Errorf("capture idle run workspace: %w", err)
	}
	captured, err = canonicalRepository(captured)
	if err != nil {
		return err
	}
	// The callback may explicitly reuse an immutable child snapshot. The host
	// owns that distinction; parent sessions must always return a private copy.
	bound := view.validateWorkflow(definition.NormalizedYAML, captured)
	if !bound.Valid {
		return validationError(bound)
	}
	if bound.PayloadDigest != definition.PayloadDigest {
		return errors.New("execution snapshot changed the approved workflow payload")
	}
	root := executionRoot(dir, a.ID)
	if _, err = os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		return errors.New("execution instance already exists; uncertain preparation is not retried")
	}
	if err = os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		return err
	}
	if _, err = instance.InitDemo(root); err != nil {
		return err
	}
	// Copy only this service's already-projected, credential-free configuration,
	// never the host instance. Each execution owns its own scheduler/journals.
	sourceRoot := filepath.Join(dir, "instance")
	instanceData, err := os.ReadFile(instance.NewLayout(sourceRoot).ConfigFile())
	if err != nil {
		return err
	}
	if err = atomicWrite(instance.NewLayout(root).ConfigFile(), instanceData); err != nil {
		return err
	}
	config := filepath.Join(root, "config")
	if err = os.RemoveAll(config); err != nil {
		return err
	}
	if err = copyTree(filepath.Join(sourceRoot, "config"), config); err != nil {
		return err
	}
	wfFile, ok := set.WorkflowSource(a.Request.Gaggle, a.Request.Workflow)
	if !ok || !safeConfigRel(wfFile) {
		return errors.New("selected workflow source is unsafe")
	}
	if err = atomicWrite(filepath.Join(config, wfFile), []byte(bound.BoundYAML)); err != nil {
		return err
	}
	if err = validateConfig(config); err != nil {
		return err
	}
	a.ExecutionRoot = root
	a.ExecutionRepository = captured
	a.ExecutionPayloadDigest = bound.PayloadDigest
	a.ExecutionBindingDigest = bound.BindingDigest
	// Persist the immutable execution identity before the caller may dispatch.
	return saveAction(dir, a)
}
