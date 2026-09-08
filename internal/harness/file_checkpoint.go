package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/platform/safeopen"
)

// fileTranscriptCheckpoint reads only newly appended bytes within a pinned,
// trusted root. Every subordinate path component refuses symlinks. A session
// log may appear after launch, but cannot disappear, shrink, or change identity
// after its first observation. The caller owns root and serializes capture.
type fileTranscriptCheckpoint struct {
	root     *os.File
	path     string
	limit    int64
	sink     func(TranscriptDelta) error
	identity os.FileInfo
	offset   int64
	size     int64
}

func (s *fileTranscriptCheckpoint) capture(reason string) error {
	f, err := openCheckpointFile(s.root, s.path)
	if errors.Is(err, os.ErrNotExist) && s.identity == nil {
		return nil // The harness has not created its native session log yet.
	}
	if err != nil {
		return fmt.Errorf("open native transcript checkpoint: %w", err)
	}
	data, info, err := s.read(f)
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if len(data) == 0 && info.Size() == s.size && reason == "checkpoint" {
		return nil
	}
	limit := s.limit
	if limit <= 0 {
		limit = DefaultMaxTranscriptBytes
	}
	delta := TranscriptDelta{Offset: int(s.offset), Data: data, DroppedBytes: max(0, info.Size()-limit), Reason: reason}
	if err := s.sink(delta); err != nil {
		return err
	}
	s.offset += int64(len(data))
	s.size = info.Size()
	s.identity = info
	return nil
}

func (s *fileTranscriptCheckpoint) read(f *os.File) ([]byte, os.FileInfo, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, errors.New("native transcript checkpoint is not a regular file")
	}
	if s.identity != nil && (!os.SameFile(s.identity, info) || info.Size() < s.size) {
		return nil, nil, errors.New("native transcript checkpoint was replaced or truncated")
	}
	limit := s.limit
	if limit <= 0 {
		limit = DefaultMaxTranscriptBytes
	}
	length := min(info.Size(), limit) - s.offset
	if length < 0 {
		return nil, nil, errors.New("native transcript checkpoint cursor exceeds file")
	}
	data := make([]byte, length)
	if length > 0 {
		if _, err := f.ReadAt(data, s.offset); err != nil {
			return nil, nil, fmt.Errorf("read native transcript checkpoint: %w", err)
		}
	}
	return data, info, nil
}

func openCheckpointFile(root *os.File, path string) (*os.File, error) {
	if !filepath.IsLocal(path) || filepath.Clean(path) == "." {
		return nil, errors.New("native transcript checkpoint path is not local")
	}
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	dir := root
	for i, part := range parts {
		next, err := safeopen.OpenAt(dir, part)
		if dir != root {
			err = errors.Join(err, dir.Close())
		}
		if err != nil {
			if next != nil {
				err = errors.Join(err, next.Close())
			}
			return nil, err
		}
		if i == len(parts)-1 {
			return next, nil
		}
		info, err := next.Stat()
		if err != nil || !info.IsDir() {
			return nil, errors.Join(err, errors.New("native transcript checkpoint parent is not a directory"), next.Close())
		}
		dir = next
	}
	return nil, errors.New("native transcript checkpoint path is empty")
}
