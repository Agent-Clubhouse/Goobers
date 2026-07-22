package instance

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

func TestLoadConfigSourceResolvesSource(t *testing.T) {
	source := &recordingConfigSource{path: validConfigDir}

	set, report, err := LoadConfigSource(context.Background(), source)
	if err != nil {
		t.Fatalf("LoadConfigSource: %v (report: %+v)", err, report)
	}
	if !source.called {
		t.Fatal("config source was not resolved")
	}
	if set.Manifest == nil {
		t.Fatal("expected a Manifest")
	}
}

func TestLoadConfigSourceReturnsResolveError(t *testing.T) {
	want := errors.New("source unavailable")
	source := &recordingConfigSource{err: want}

	set, report, err := LoadConfigSource(context.Background(), source)
	if !errors.Is(err, want) {
		t.Fatalf("LoadConfigSource error = %v, want %v", err, want)
	}
	if set != nil || report != nil {
		t.Fatalf("LoadConfigSource = (%+v, %+v), want nil set and report", set, report)
	}
}
