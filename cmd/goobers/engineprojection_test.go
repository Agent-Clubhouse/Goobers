package main

import (
	"context"
	"os"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestEngineProjectionIsInertWithoutTemporalConfiguration(t *testing.T) {
	t.Setenv("GOOBERS_TEMPORAL_HOSTPORT", "")
	root := t.TempDir()
	stop, err := startEngineProjection(context.Background(), instance.NewLayout(root), nil, nil, nil)
	if err != nil {
		t.Fatalf("startEngineProjection: %v", err)
	}
	stop()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unconfigured projection changed instance root: %v", entries)
	}
}
