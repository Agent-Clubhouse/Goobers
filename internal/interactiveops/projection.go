package interactiveops

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// projectConfig derives an execution-only native config snapshot. Registration
// remains in the host config; spec.interactive is removed from this projection
// so its broader chat references cannot widen the session workflow allowlist.
func (s *Service) projectConfig(scope Scope, dst, repo string, p Policy) error {
	src, err := s.configDir(scope)
	if err != nil {
		return err
	}
	set, err := loadNativeConfig(src)
	if err != nil {
		return fmt.Errorf("native configuration source: %w", err)
	}
	manifest := *set.Manifest
	manifest.Spec.Gaggles = []string{p.Gaggle}
	var gaggle *apiv1.Gaggle
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == p.Gaggle {
			gaggle = &set.Gaggles[i]
			break
		}
	}
	if gaggle == nil {
		return errors.New("session gaggle is absent from the validated configuration")
	}
	// Connection references are configuration names, never token payloads. Copy
	// only referenced declarations; no instance.yaml/secrets/credential file.
	connections := map[string]bool{gaggle.Spec.Project.ConnectionRef: true, gaggle.Spec.Backlog.ConnectionRef: true}
	for _, r := range gaggle.Spec.AdditionalRepos {
		connections[r.ConnectionRef] = true
	}
	manifest.Spec.Connections = nil
	for _, c := range set.Manifest.Spec.Connections {
		if connections[c.Name] {
			manifest.Spec.Connections = append(manifest.Spec.Connections, c)
		}
	}
	if err = writeProjected(dst, "manifest.yaml", manifest, false); err != nil {
		return err
	}
	if err = writeProjected(dst, "gaggle.yaml", gaggle, true); err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, name := range p.Workflows {
		wanted[name] = true
	}
	gooberNames := map[string]bool{}
	// The selected gaggle's configured personas are context even when the
	// bounded workflow is deterministic. Only same-gaggle or referenced shared
	// personas are copied; their workflow backlinks are narrowed below.
	for _, g := range set.Goobers {
		if g.Spec.Gaggle == p.Gaggle {
			gooberNames[g.Name] = true
		}
	}
	if gaggle.Spec.Interactive != nil {
		for _, surface := range gaggle.Spec.Interactive.Surfaces {
			gooberNames[surface.GooberRef] = true
		}
	}
	view, _, err := s.scoped(scope)
	if err != nil {
		return err
	}
	if err = view.projectWorkflows(set, dst, repo, p, wanted, gooberNames); err != nil {
		return err
	}
	if err = projectGoobers(set, src, dst, p, gooberNames); err != nil {
		return err
	}
	if err = validateConfig(dst); err != nil {
		return fmt.Errorf("filtered native configuration is unsupported: %w", err)
	}
	return nil
}

func (s *Service) projectWorkflows(set *instance.ConfigSet, dst, repo string, p Policy, wanted, gooberNames map[string]bool) error {
	for _, wf := range set.Workflows {
		if wf.Spec.Gaggle != p.Gaggle || !wanted[wf.Name] {
			continue
		}
		delete(wanted, wf.Name)
		for _, task := range wf.Spec.Tasks {
			if task.Goober != "" {
				gooberNames[task.Goober] = true
			}
			if task.ChildWorkflows != nil {
				for _, g := range task.ChildWorkflows.AllowedGoobers {
					gooberNames[g] = true
				}
			}
		}
		for _, gate := range wf.Spec.Gates {
			if gate.Agentic != nil {
				gooberNames[gate.Agentic.Goober] = true
			}
		}
		data, err := marshalConfig(wf)
		if err != nil {
			return err
		}
		// Only the bounded subset is rebound to the current workspace. Other
		// configured workflows remain visible but submit validation refuses them.
		if v := s.validateWorkflow(string(data), repo); v.Valid {
			data = []byte(v.BoundYAML)
		}
		if err := os.WriteFile(filepath.Join(dst, "workflow-"+wf.Name+".yaml"), data, 0600); err != nil {
			return err
		}
	}
	if len(wanted) > 0 {
		return errors.New("policy references a workflow absent from the selected gaggle")
	}
	return nil
}

func projectGoobers(set *instance.ConfigSet, src, dst string, p Policy, gooberNames map[string]bool) error {
	for _, g := range set.Goobers {
		if !gooberNames[g.Name] {
			continue
		}
		if g.Spec.Gaggle != "" && g.Spec.Gaggle != p.Gaggle {
			return errors.New("cross-gaggle goober reference is not authorized")
		}
		source, ok := set.GooberSource(g.Name)
		if !ok {
			return errors.New("referenced goober source unavailable")
		}
		if !safeConfigRel(g.Spec.Instructions) {
			return errors.New("unsafe goober instruction path")
		}
		assetRoot, sourceDir, err := gooberAssetSource(src, source, g.Name)
		if err != nil {
			return err
		}
		// Rehome shared personas into this isolated gaggle, preserving only the
		// selected workflow backlinks. Source instruction content stays unchanged.
		g.Spec.Gaggle = p.Gaggle
		visible := []string{}
		for _, name := range g.Spec.Workflows {
			for _, allowed := range p.Workflows {
				if name == allowed {
					visible = append(visible, name)
				}
			}
		}
		g.Spec.Workflows = visible
		targetDir := filepath.Join("gaggles", p.Gaggle, "goobers", g.Name)
		if err = copyConfigFile(assetRoot, filepath.Join(sourceDir, g.Spec.Instructions), dst, filepath.Join(targetDir, g.Spec.Instructions)); err != nil {
			return fmt.Errorf("goober instructions: %w", err)
		}
		if err = copyConfigAssetTree(assetRoot, filepath.Join(sourceDir, "assets"), dst, filepath.Join(targetDir, "assets")); err != nil {
			return err
		}
		for _, skill := range g.Spec.Skills {
			if !namePattern.MatchString(skill) {
				return errors.New("unsafe skill package name")
			}
			rel := filepath.Join("gaggles", p.Gaggle, "skills", skill)
			if err = copyConfigAssetTree(src, rel, dst, rel); err != nil {
				return err
			}
		}
		if err = writeProjected(dst, filepath.Join(targetDir, "goober.yaml"), g, false); err != nil {
			return err
		}
		delete(gooberNames, g.Name)
	}
	if len(gooberNames) > 0 {
		return errors.New("selected workflow references an unavailable goober")
	}
	return nil
}

// Native configuration permits shared personas in the single sibling goobers/
// root. Select that root explicitly from validated provenance; never loosen the
// relative asset guard to permit arbitrary parent traversal.
func gooberAssetSource(config, source, name string) (string, string, error) {
	root, rel := config, source
	sharedPrefix := filepath.Join("..", "goobers") + string(filepath.Separator)
	if strings.HasPrefix(source, sharedPrefix) {
		rel = strings.TrimPrefix(source, sharedPrefix)
		if filepath.Dir(rel) != name || !namePattern.MatchString(name) {
			return "", "", errors.New("shared goober source does not match its persona")
		}
		root = filepath.Join(filepath.Dir(config), "goobers")
	}
	if !safeConfigRel(rel) {
		return "", "", errors.New("unsafe goober source provenance")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", errors.New("goober source root must be an ordinary directory")
	}
	return root, filepath.Dir(rel), nil
}
func writeProjected(root, rel string, value any, registration bool) error {
	data, err := marshalConfig(value)
	if err != nil {
		return err
	}
	if registration {
		var raw map[string]any
		if err = yaml.UnmarshalStrict(data, &raw); err != nil {
			return err
		}
		spec, _ := raw["spec"].(map[string]any)
		delete(spec, "interactive")
		// The serialized native object retains its DSL pin and every other field.
		data, err = yaml.Marshal(raw)
		if err != nil {
			return err
		}
	}
	if !safeConfigRel(rel) {
		return errors.New("unsafe projected config destination")
	}
	path := filepath.Join(root, rel)
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
func safeConfigRel(rel string) bool {
	if rel == "" || filepath.IsAbs(rel) || filepath.Clean(rel) != rel || strings.ContainsAny(rel, "\\\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		lower := strings.ToLower(part)
		if part == "." || part == ".." || strings.HasPrefix(part, ".") || lower == "secrets" || lower == "credentials" || lower == "instance.yaml" || lower == "instance.yml" || strings.HasSuffix(lower, ".pem") || strings.HasSuffix(lower, ".key") || strings.HasSuffix(lower, ".pfx") {
			return false
		}
	}
	return true
}
func copyConfigFile(src, rel, dst, target string) error {
	if !safeConfigRel(rel) || !safeConfigRel(target) {
		return errors.New("unsafe credential/metadata/config asset path")
	}
	at := src
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		at = filepath.Join(at, part)
		info, err := os.Lstat(at)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink config assets are forbidden")
		}
	}
	info, err := os.Stat(at)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return errors.New("config asset must be a regular file up to 4 MiB")
	}
	data, err := os.ReadFile(at)
	if err != nil {
		return err
	}
	// Config assets must never be a source of credential payloads. Known secret
	// file forms are refused; the host still owns source classification.
	if strings.Contains(string(data), "-----BEGIN PRIVATE KEY-----") || strings.Contains(string(data), "-----BEGIN RSA PRIVATE KEY-----") {
		return errors.New("private key material is forbidden in config assets")
	}
	path := filepath.Join(dst, target)
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
func copyConfigAssetTree(src, rel, dst, target string) error {
	if !safeConfigRel(rel) || !safeConfigRel(target) {
		return errors.New("unsafe asset tree")
	}
	path := filepath.Join(src, rel)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var count, total int64
	return filepath.WalkDir(path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		suffix, err := filepath.Rel(filepath.Join(src, rel), path)
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink config asset tree forbidden")
		}
		if d.IsDir() {
			if !safeConfigRel(filepath.Join(rel, suffix)) {
				return errors.New("credential/metadata asset tree forbidden")
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		count++
		total += info.Size()
		if count > 256 || total > 16<<20 {
			return errors.New("config asset tree exceeds bounded snapshot size")
		}
		return copyConfigFile(src, filepath.Join(rel, suffix), dst, filepath.Join(target, suffix))
	})
}
