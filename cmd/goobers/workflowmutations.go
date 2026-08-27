package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
)

// workflowMutationService implements httpapi.WorkflowMutationService: it
// rewrites a workflow's YAML source file's non-manual triggers' `enabled`
// field and then hot-reloads the daemon from the edited config directory
// (#WF-enable-disable). Unlike the run-intervention service, this mutates
// config a daemon may not have been started with --watch-config to observe
// automatically, so it drives reloader.pollOnce itself.
//
// reloader is attached after construction (AttachReloader), mirroring
// runInterventionService.AttachScheduler — up.go wires the HTTP handler
// before the reloader exists, so this service is constructed first and the
// reloader filled in once it's built.
type workflowMutationService struct {
	layout   instance.Layout
	reloader atomic.Pointer[configReloader]
	// mu serializes concurrent toggle requests so a read-modify-write of the
	// same file (or a racing reload) can't interleave.
	mu sync.Mutex
}

func newWorkflowMutationService(l instance.Layout) *workflowMutationService {
	return &workflowMutationService{layout: l}
}

func (s *workflowMutationService) AttachReloader(reloader *configReloader) {
	if s != nil {
		s.reloader.Store(reloader)
	}
}

func (s *workflowMutationService) SetWorkflowEnabled(ctx context.Context, input httpapi.WorkflowEnabledRequest) (httpapi.WorkflowEnabledResult, error) {
	reloader := s.reloader.Load()
	if reloader == nil {
		return httpapi.WorkflowEnabledResult{}, httpapi.NewInterventionError(
			http.StatusServiceUnavailable, "workflow_mutations_unavailable", "workflow config mutations are not available yet", nil)
	}
	gaggle := strings.TrimSpace(input.Gaggle)
	name := strings.TrimSpace(input.Workflow)
	if gaggle == "" || name == "" {
		return httpapi.WorkflowEnabledResult{}, httpapi.NewInterventionError(
			http.StatusBadRequest, "invalid_request", "gaggle and workflow are required", nil)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Reload current setup.Definitions is read under the reloader's own lock
	// (poll/pollOnce hold it while mutating it), so the pointer read below is
	// safe: it either observes the previous or the just-applied definitions,
	// never a half-written one.
	reloader.mu.Lock()
	set := reloader.setup.Definitions
	reloader.mu.Unlock()
	source, ok := set.WorkflowSource(gaggle, name)
	if !ok {
		return httpapi.WorkflowEnabledResult{}, httpapi.NewInterventionError(
			http.StatusNotFound, "workflow_not_found", "workflow was not found", nil)
	}
	path := filepath.Join(s.layout.ConfigDir(), source)

	raw, err := os.ReadFile(path)
	if err != nil {
		return httpapi.WorkflowEnabledResult{}, fmt.Errorf("read workflow source %s: %w", path, err)
	}
	edited, changed, err := setNonManualTriggersEnabled(raw, input.Enabled)
	if err != nil {
		return httpapi.WorkflowEnabledResult{}, httpapi.NewInterventionError(
			http.StatusUnprocessableEntity, "workflow_edit_failed", err.Error(), err)
	}
	if changed {
		// Preserve the source file's permissions and write atomically enough
		// for this single-writer (mutex-serialized) path: same directory,
		// then rename over the original.
		info, statErr := os.Stat(path)
		mode := os.FileMode(0o644)
		if statErr == nil {
			mode = info.Mode()
		}
		tmp := path + ".tmp-" + fmt.Sprint(time.Now().UnixNano())
		if err := os.WriteFile(tmp, edited, mode); err != nil {
			return httpapi.WorkflowEnabledResult{}, fmt.Errorf("write workflow source %s: %w", path, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return httpapi.WorkflowEnabledResult{}, fmt.Errorf("replace workflow source %s: %w", path, err)
		}
	}

	// Hot-reload now rather than waiting for --watch-config's own ticker (which
	// may not even be running) — same on-demand contract `goobers apply` uses.
	if _, _, _, rejected, err := reloader.pollOnce(time.Now()); err != nil {
		return httpapi.WorkflowEnabledResult{}, fmt.Errorf("reload config after workflow edit: %w", err)
	} else if rejected != "" {
		return httpapi.WorkflowEnabledResult{}, httpapi.NewInterventionError(
			http.StatusUnprocessableEntity, "workflow_edit_rejected", rejected, nil)
	}

	return httpapi.WorkflowEnabledResult{Gaggle: gaggle, Workflow: name, Enabled: input.Enabled}, nil
}

// setNonManualTriggersEnabled edits spec.triggers[].enabled for every trigger
// whose type is not "manual", leaving type=manual triggers, comments, key
// order, and everything else in the document untouched. It round-trips
// through yaml.v3's Node tree (rather than unmarshal-to-struct then
// re-marshal) specifically so untouched nodes keep their original comments
// and formatting — workflow YAML files carry substantial prose documentation
// that a full re-marshal would silently discard.
func setNonManualTriggersEnabled(raw []byte, enabled bool) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, false, fmt.Errorf("parse workflow YAML: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, false, fmt.Errorf("workflow YAML has no document root")
	}
	root := doc.Content[0]
	spec := mappingValue(root, "spec")
	if spec == nil {
		return nil, false, fmt.Errorf("workflow YAML has no spec")
	}
	triggers := mappingValue(spec, "triggers")
	if triggers == nil || triggers.Kind != yaml.SequenceNode {
		return nil, false, fmt.Errorf("workflow YAML has no spec.triggers list")
	}
	changed := false
	for _, trigger := range triggers.Content {
		if trigger.Kind != yaml.MappingNode {
			continue
		}
		if typeVal := mappingValue(trigger, "type"); typeVal == nil || typeVal.Value == "manual" {
			continue
		}
		if setMappingBool(trigger, "enabled", enabled) {
			changed = true
		}
	}
	if !changed {
		return raw, false, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	// 2-space indent matches goobers' scaffolded/hand-authored workflow YAML
	// convention; yaml.Marshal's default of 4 spaces would reformat every
	// existing line in the file, not just the edited trigger.
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, false, fmt.Errorf("re-marshal workflow YAML: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, false, fmt.Errorf("re-marshal workflow YAML: %w", err)
	}
	return buf.Bytes(), true, nil
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// setMappingBool sets key: value in a mapping node, appending a new key if
// absent, or overwriting the existing scalar's value if the desired boolean
// already differs. Returns whether it changed anything.
func setMappingBool(mapping *yaml.Node, key string, value bool) bool {
	want := "false"
	if value {
		want = "true"
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			v := mapping.Content[i+1]
			if v.Value == want {
				return false
			}
			v.Value = want
			v.Tag = "!!bool"
			return true
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	valNode := &yaml.Node{Kind: yaml.ScalarNode, Value: want, Tag: "!!bool"}
	mapping.Content = append(mapping.Content, keyNode, valNode)
	return true
}
