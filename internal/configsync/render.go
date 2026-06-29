package configsync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// WriteManifests renders the desired CR set into outDir as one YAML file per
// object (`<kind>-<name>.yaml`). This is the default GitOps path: ArgoCD watches
// outDir and applies/prunes from it. To make removal work, WriteManifests first
// clears previously-rendered files in outDir, so an object dropped from the
// config repo disappears from the render and ArgoCD prunes it from the cluster.
func (rs *RenderSet) WriteManifests(outDir string) ([]string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", outDir, err)
	}
	if err := clearRendered(outDir); err != nil {
		return nil, err
	}

	var written []string
	for _, obj := range rs.Objects {
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		name := obj.GetName()
		data, err := yaml.Marshal(obj)
		if err != nil {
			return nil, fmt.Errorf("marshal %s/%s: %w", kind, name, err)
		}
		fname := fmt.Sprintf("%s-%s.yaml", strings.ToLower(kind), name)
		path := filepath.Join(outDir, fname)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
		written = append(written, fname)
	}
	return written, nil
}

// clearRendered removes previously-rendered manifests (top-level *.yaml) so the
// output directory reflects exactly the current desired state.
func clearRendered(outDir string) error {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", outDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml") {
			if err := os.Remove(filepath.Join(outDir, e.Name())); err != nil {
				return fmt.Errorf("remove stale %s: %w", e.Name(), err)
			}
		}
	}
	return nil
}
