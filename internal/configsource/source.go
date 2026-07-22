// Package configsource defines the source boundary used by config loaders.
package configsource

import "context"

// ConfigSource resolves the current config snapshot to a directory.
type ConfigSource interface {
	Resolve(context.Context) (string, error)
}

// LocalDirSource resolves to a plain directory without modifying or copying it.
type LocalDirSource struct {
	Path string
}

// Resolve returns the configured local directory.
func (s LocalDirSource) Resolve(context.Context) (string, error) {
	return s.Path, nil
}
