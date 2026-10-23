package journal

import (
	"context"
	"path/filepath"
)

func (b *fileBackend) RunRecorded(_ context.Context, key RunKey) (bool, error) {
	root, err := b.runsDir(key.Gaggle)
	if err != nil {
		return false, err
	}
	return Recorded(filepath.Join(root, key.RunID)), nil
}

func (b *fileBackend) OpenRunReader(_ context.Context, key RunKey) (RunReader, error) {
	root, err := b.runsDir(key.Gaggle)
	if err != nil {
		return nil, err
	}
	return OpenReadOnly(filepath.Join(root, key.RunID))
}
