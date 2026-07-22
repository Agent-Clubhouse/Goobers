package configsource

import (
	"context"
	"testing"
)

func TestLocalDirSourceResolve(t *testing.T) {
	source := LocalDirSource{Path: "does/not/need/to/exist"}

	got, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != source.Path {
		t.Fatalf("Resolve() = %q, want %q", got, source.Path)
	}
}
