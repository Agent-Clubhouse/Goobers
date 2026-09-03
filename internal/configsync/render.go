package configsync

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/platform/durability"
)

const generationMetadata = ".config-sync-generation"

type renderedManifest struct {
	name string
	data []byte
}

type publicationHooks struct {
	beforeGenerationWrite      func() error
	duringGenerationWrite      func() error
	beforeGenerationValidation func(string) error
	afterGenerationPublish     func() error
	beforeAuthoritativeSwap    func() error
	syncAuthoritativeSwitch    func(string) error
}

// ManifestGenerationDir returns the sibling directory that stores immutable
// generations for an authoritative manifest output path.
func ManifestGenerationDir(outDir string) string {
	parent, base := filepath.Split(filepath.Clean(outDir))
	return filepath.Join(parent, "."+base+".generations")
}

// WriteManifests renders the desired CR set as an immutable sibling generation,
// then atomically switches outDir to that complete generation. The previous
// generation remains authoritative if any pre-publication operation fails.
func (rs *RenderSet) WriteManifests(outDir string) ([]string, error) {
	return rs.writeManifests(outDir, publicationHooks{})
}

func (rs *RenderSet) writeManifests(outDir string, hooks publicationHooks) ([]string, error) {
	manifests, err := rs.renderManifests()
	if err != nil {
		return nil, err
	}

	outDir = filepath.Clean(outDir)
	parent := filepath.Dir(outDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create manifest output parent %s: %w", parent, err)
	}
	if err := validateManifestOutput(outDir); err != nil {
		return nil, err
	}

	generations := ManifestGenerationDir(outDir)
	if err := os.MkdirAll(generations, 0o755); err != nil {
		return nil, fmt.Errorf("create manifest generation directory %s: %w", generations, err)
	}
	if err := durability.SyncDir(parent); err != nil {
		return nil, fmt.Errorf("sync manifest output parent %s: %w", parent, err)
	}

	staging, err := os.MkdirTemp(generations, ".staging-")
	if err != nil {
		return nil, fmt.Errorf("create manifest staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := callPublicationHook(hooks.beforeGenerationWrite); err != nil {
		return nil, fmt.Errorf("before manifest generation write: %w", err)
	}
	for _, manifest := range manifests {
		if err := writeDurableFile(filepath.Join(staging, manifest.name), manifest.data, 0o644); err != nil {
			return nil, fmt.Errorf("write manifest %s: %w", manifest.name, err)
		}
		if err := callPublicationHook(hooks.duringGenerationWrite); err != nil {
			return nil, fmt.Errorf("during manifest generation write: %w", err)
		}
	}

	generationName := "generation-" + strings.TrimPrefix(filepath.Base(staging), ".staging-")
	metadata := generationMetadataContent(generationName, manifests)
	if err := writeDurableFile(filepath.Join(staging, generationMetadata), metadata, 0o644); err != nil {
		return nil, fmt.Errorf("write manifest generation metadata: %w", err)
	}
	if hooks.beforeGenerationValidation != nil {
		if err := hooks.beforeGenerationValidation(staging); err != nil {
			return nil, fmt.Errorf("before manifest generation validation: %w", err)
		}
	}
	if err := validateManifestGeneration(staging, manifests, metadata); err != nil {
		return nil, fmt.Errorf("validate manifest generation: %w", err)
	}
	if err := durability.SyncDir(staging); err != nil {
		return nil, fmt.Errorf("sync manifest generation: %w", err)
	}

	generation := filepath.Join(generations, generationName)
	if err := durability.Move(staging, generation); err != nil {
		return nil, fmt.Errorf("publish manifest generation: %w", err)
	}
	if err := durability.SyncDir(generations); err != nil {
		return nil, fmt.Errorf("sync published manifest generation: %w", err)
	}
	if err := callPublicationHook(hooks.afterGenerationPublish); err != nil {
		return nil, fmt.Errorf("after manifest generation publication: %w", err)
	}

	target, err := filepath.Rel(parent, generation)
	if err != nil {
		return nil, fmt.Errorf("resolve manifest generation target: %w", err)
	}
	pointer := filepath.Join(parent, "."+filepath.Base(outDir)+".publish-"+generationName)
	if err := os.Symlink(target, pointer); err != nil {
		return nil, fmt.Errorf("prepare manifest generation pointer: %w", err)
	}
	defer func() { _ = os.RemoveAll(pointer) }()
	if err := durability.SyncDir(parent); err != nil {
		return nil, fmt.Errorf("sync manifest publication metadata: %w", err)
	}
	if err := callPublicationHook(hooks.beforeAuthoritativeSwap); err != nil {
		return nil, fmt.Errorf("before authoritative manifest switch: %w", err)
	}

	rollback, err := switchManifestOutput(pointer, outDir, generations, generationName)
	if err != nil {
		return nil, err
	}
	syncAuthoritativeSwitch := durability.SyncDir
	if hooks.syncAuthoritativeSwitch != nil {
		syncAuthoritativeSwitch = hooks.syncAuthoritativeSwitch
	}
	if err := syncAuthoritativeSwitch(parent); err != nil {
		rollbackErr := rollback()
		if rollbackErr != nil {
			return nil, fmt.Errorf("sync authoritative manifest switch: %w (rollback failed: %w)", err, rollbackErr)
		}
		return nil, fmt.Errorf("sync authoritative manifest switch: %w", err)
	}

	written := make([]string, 0, len(manifests))
	for _, manifest := range manifests {
		written = append(written, manifest.name)
	}
	return written, nil
}

func (rs *RenderSet) renderManifests() ([]renderedManifest, error) {
	manifests := make([]renderedManifest, 0, len(rs.Objects))
	for _, obj := range rs.Objects {
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		name := obj.GetName()
		data, err := yaml.Marshal(obj)
		if err != nil {
			return nil, fmt.Errorf("marshal %s/%s: %w", kind, name, err)
		}
		manifests = append(manifests, renderedManifest{
			name: fmt.Sprintf("%s-%s.yaml", strings.ToLower(kind), name),
			data: data,
		})
	}
	return manifests, nil
}

func validateManifestOutput(outDir string) error {
	info, err := os.Lstat(outDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect manifest output %s: %w", outDir, err)
	}
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	return fmt.Errorf("manifest output %s must be a directory or symbolic link", outDir)
}

func writeDurableFile(path string, data []byte, mode fs.FileMode) (retErr error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); retErr == nil && err != nil {
			retErr = err
		}
	}()
	n, err := file.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return fmt.Errorf("short write: wrote %d of %d bytes", n, len(data))
	}
	return file.Sync()
}

func generationMetadataContent(generationName string, manifests []renderedManifest) []byte {
	var metadata strings.Builder
	fmt.Fprintf(&metadata, "generation=%s\n", generationName)
	for _, manifest := range manifests {
		fmt.Fprintf(&metadata, "manifest=%s\n", manifest.name)
	}
	return []byte(metadata.String())
}

func validateManifestGeneration(dir string, manifests []renderedManifest, metadata []byte) error {
	expected := make(map[string][]byte, len(manifests)+1)
	expected[generationMetadata] = metadata
	for _, manifest := range manifests {
		expected[manifest.name] = manifest.data
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != len(expected) {
		return fmt.Errorf("contains %d files, want %d", len(entries), len(expected))
	}
	for _, entry := range entries {
		want, ok := expected[entry.Name()]
		if !ok {
			return fmt.Errorf("contains unexpected file %s", entry.Name())
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", entry.Name())
		}
		got, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("%s content does not match rendered manifest", entry.Name())
		}
	}
	return nil
}

func switchManifestOutput(pointer, outDir, generations, generationName string) (func() error, error) {
	info, err := os.Lstat(outDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := durability.Move(pointer, outDir); err != nil {
			return nil, fmt.Errorf("publish authoritative manifest output: %w", err)
		}
		return func() error {
			if err := os.Remove(outDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return durability.SyncDir(filepath.Dir(outDir))
		}, nil
	case err != nil:
		return nil, fmt.Errorf("inspect authoritative manifest output: %w", err)
	case info.Mode()&os.ModeSymlink != 0:
		oldTarget, err := os.Readlink(outDir)
		if err != nil {
			return nil, fmt.Errorf("read current manifest generation pointer: %w", err)
		}
		if !filepath.IsAbs(oldTarget) {
			oldTarget = filepath.Join(filepath.Dir(outDir), oldTarget)
		}
		rollbackTarget, err := filepath.Rel(filepath.Dir(outDir), oldTarget)
		if err != nil {
			return nil, fmt.Errorf("resolve manifest publication rollback target: %w", err)
		}
		rollbackPointer := filepath.Join(generations, ".rollback-"+generationName)
		if err := os.Symlink(rollbackTarget, rollbackPointer); err != nil {
			return nil, fmt.Errorf("prepare manifest publication rollback: %w", err)
		}
		if err := durability.SyncDir(generations); err != nil {
			return nil, fmt.Errorf("sync manifest publication rollback: %w", err)
		}
		if err := replaceManifestPointer(pointer, outDir); err != nil {
			return nil, fmt.Errorf("publish authoritative manifest output: %w", err)
		}
		return func() error {
			if err := replaceManifestPointer(rollbackPointer, outDir); err != nil {
				return err
			}
			return durability.SyncDir(filepath.Dir(outDir))
		}, nil
	case info.IsDir():
		if err := swapManifestPaths(pointer, outDir); err != nil {
			return nil, fmt.Errorf("atomically migrate manifest output directory: %w", err)
		}
		return func() error {
			if err := swapManifestPaths(pointer, outDir); err != nil {
				return err
			}
			return durability.SyncDir(filepath.Dir(outDir))
		}, nil
	default:
		return nil, fmt.Errorf("manifest output %s must be a directory or symbolic link", outDir)
	}
}

func replaceManifestPointer(source, destination string) error {
	if err := os.Remove(destination); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Rename(source, destination)
}

func callPublicationHook(hook func() error) error {
	if hook == nil {
		return nil
	}
	return hook()
}
