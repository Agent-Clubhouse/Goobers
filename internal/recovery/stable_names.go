package recovery

import (
	"errors"
	"io"
	"os"
	"slices"
)

type listNamesOptions struct {
	readLimit        int
	changedRootError string
	ignoredForCount  []string
	maxCount         int
	fullError        func(count int) error
}

func listStableNames(root string, before os.FileInfo, opts listNamesOptions) ([]string, error) {
	file, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, errors.New(opts.changedRootError)
	}
	names, err := file.Readdirnames(opts.readLimit)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	count := len(names)
	for _, ignored := range opts.ignoredForCount {
		if slices.Contains(names, ignored) {
			count--
		}
	}
	if opts.fullError != nil && count > opts.maxCount {
		return nil, opts.fullError(count)
	}
	slices.Sort(names)
	return names, nil
}
