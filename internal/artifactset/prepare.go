package artifactset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/platform/safeopen"
)

// Manifest is the workspace-relative staging request, not a source of trusted
// pointers. It is never itself published into the downstream result.
type Manifest struct {
	SchemaVersion string          `json:"schemaVersion"`
	Entries       []ManifestEntry `json:"entries"`
}

// ManifestEntry requests one semantic payload from the stage workspace.
type ManifestEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	MediaType string `json:"mediaType"`
}

// Sanitize is a runner-owned, media-aware policy. It must reject unsupported or
// unsafe payloads, and decode containers before scrubbing their contents. A raw
// byte scrub of compressed data is not an acceptable implementation.
type Sanitize func(mediaType string, data []byte) ([]byte, error)

// Prepared is a fully validated, sanitized set. Private fields prevent callers
// from accidentally publishing an agent-authored pointer or unvalidated entry.
type Prepared struct{ entries []preparedEntry }

type preparedEntry struct {
	name      string
	mediaType string
	data      []byte
}

// Prepare validates every entry and sanitizes every payload before the caller
// can publish anything. Both raw and sanitized bytes have aggregate bounds.
func Prepare(ctx context.Context, workspace, manifestPath string, sanitize Sanitize) (prepared *Prepared, retErr error) {
	if sanitize == nil {
		return nil, fmt.Errorf("%w: missing sanitization policy", ErrInvalid)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := root.Close(); err != nil {
			prepared = nil
			retErr = errors.Join(retErr, err)
		}
	}()
	data, err := readWorkspaceFile(ctx, root, manifestPath, MaxIndexBytes)
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := decodeDocument(data, &manifest); err != nil {
		return nil, err
	}
	if err := validateManifest(manifest); err != nil {
		return nil, err
	}
	sort.Slice(manifest.Entries, func(i, j int) bool { return manifest.Entries[i].Name < manifest.Entries[j].Name })
	result := &Prepared{entries: make([]preparedEntry, 0, len(manifest.Entries))}
	var rawTotal, cleanTotal int64
	for _, entry := range manifest.Entries {
		data, err := readWorkspaceFile(ctx, root, entry.Path, min(MaxPayloadBytes, MaxSetBytes-rawTotal))
		if err != nil {
			return nil, err
		}
		rawTotal += int64(len(data))
		clean, err := sanitize(entry.MediaType, data)
		if err != nil {
			return nil, fmt.Errorf("%w: payload rejected by sanitization policy", ErrInvalid)
		}
		cleanTotal += int64(len(clean))
		if len(clean) > MaxPayloadBytes || cleanTotal > MaxSetBytes {
			return nil, fmt.Errorf("%w: sanitized set exceeds byte limit", ErrInvalid)
		}
		result.entries = append(result.entries, preparedEntry{name: entry.Name, mediaType: entry.MediaType, data: append([]byte(nil), clean...)})
	}
	return result, nil
}

func validateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != SchemaVersion || manifest.Entries == nil || len(manifest.Entries) > MaxEntries {
		return fmt.Errorf("%w: manifest schema or entry count", ErrInvalid)
	}
	seen := make(map[string]bool, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		if !semanticName.MatchString(entry.Name) || seen[entry.Name] || entry.MediaType == "" || len(entry.MediaType) > 128 {
			return fmt.Errorf("%w: invalid manifest entry", ErrInvalid)
		}
		seen[entry.Name] = true
		// Validate the path without touching the filesystem; the subsequent
		// os.Root open supplies race-safe symlink containment.
		if err := (apiv1.ArtifactPointer{Path: entry.Path, Digest: apiv1.Digest(nil)}).Validate(); err != nil {
			return fmt.Errorf("%w: invalid workspace path", ErrInvalid)
		}
	}
	return nil
}

func readWorkspaceFile(ctx context.Context, root *os.Root, path string, limit int64) (data []byte, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := (apiv1.ArtifactPointer{Path: path, Digest: apiv1.Digest(nil)}).Validate(); err != nil {
		return nil, fmt.Errorf("%w: invalid workspace path", ErrInvalid)
	}
	file, err := safeopen.OpenRegularInRoot(root, path)
	if err != nil {
		return nil, fmt.Errorf("%w: unreadable workspace artifact", ErrInvalid)
	}
	defer func() {
		if err := file.Close(); err != nil {
			data = nil
			retErr = errors.Join(retErr, err)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%w: workspace artifact exceeds byte limit", ErrInvalid)
	}
	data, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: workspace artifact exceeds byte limit", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}
