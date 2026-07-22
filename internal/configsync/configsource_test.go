package configsync

import (
	"context"
	"errors"
	"testing"
)

type recordingConfigSource struct {
	path   string
	err    error
	called bool
}

func (s *recordingConfigSource) Resolve(context.Context) (string, error) {
	s.called = true
	return s.path, s.err
}

func TestLoaderLoadSourceResolvesSource(t *testing.T) {
	loader, err := NewLoader("")
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	source := &recordingConfigSource{path: validConfigRepo}

	set, report, err := loader.LoadSource(context.Background(), source)
	if err != nil {
		t.Fatalf("LoadSource: %v (report: %+v)", err, report)
	}
	if !source.called {
		t.Fatal("config source was not resolved")
	}
	if set.Manifest == nil {
		t.Fatal("expected a Manifest")
	}
}

func TestLoaderLoadSourceReturnsResolveError(t *testing.T) {
	loader, err := NewLoader("")
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	want := errors.New("source unavailable")
	source := &recordingConfigSource{err: want}

	set, report, err := loader.LoadSource(context.Background(), source)
	if !errors.Is(err, want) {
		t.Fatalf("LoadSource error = %v, want %v", err, want)
	}
	if set != nil || report != nil {
		t.Fatalf("LoadSource = (%+v, %+v), want nil set and report", set, report)
	}
}
