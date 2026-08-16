package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestEngineProjectionIsInertWithoutTemporalConfiguration(t *testing.T) {
	root := t.TempDir()
	stop, err := startEngineProjection(context.Background(), instance.NewLayout(root), &instance.Config{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("startEngineProjection: %v", err)
	}
	stop()
}
