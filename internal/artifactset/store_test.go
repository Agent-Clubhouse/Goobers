package artifactset

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
)

type boundedStoreStub struct {
	data  []byte
	err   error
	calls int
}

func (s *boundedStoreStub) GetBounded(context.Context, string, int64) ([]byte, error) {
	s.calls++
	return s.data, s.err
}

func TestStoreReaderRestrictsCurrentRunPointers(t *testing.T) {
	data := []byte("evidence")
	p := apiv1.ArtifactPointer{Path: "artifacts/report", Digest: apiv1.Digest(data), MediaType: "text/plain", Size: int64(len(data))}
	s := &boundedStoreStub{data: data}
	r, err := NewStoreReader(s, []apiv1.ContextPointer{{Name: "stage.artifact[1]", Artifact: &p}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.ReadArtifact(context.Background(), p, 100); err != nil || string(got) != string(data) {
		t.Fatalf("read: %q, %v", got, err)
	}
	changed := p
	changed.Path = "artifacts/other"
	if got, err := r.ReadArtifact(context.Background(), changed, 100); got != nil || !errors.Is(err, ErrInvalid) || s.calls != 1 {
		t.Fatalf("unauthorized read: %q, %v; calls=%d", got, err, s.calls)
	}
	r, err = NewStoreReader(s, []apiv1.ContextPointer{{Name: "stage.artifact[1]", Artifact: &p, RunID: "another-run"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.ReadArtifact(context.Background(), p, 100); got != nil || !errors.Is(err, ErrInvalid) || s.calls != 1 {
		t.Fatalf("cross-run read: %q, %v", got, err)
	}
}

func TestStoreReaderVerifiesBytesAndFailureClass(t *testing.T) {
	data := []byte("evidence")
	p := apiv1.ArtifactPointer{Path: "artifacts/report", Digest: apiv1.Digest(data), Size: int64(len(data))}
	infra := errors.New("store offline")
	for _, failure := range []error{nil, blobstore.ErrNotFound, blobstore.ErrTooLarge, infra} {
		s := &boundedStoreStub{data: []byte("tampered"), err: failure}
		r, err := NewStoreReader(s, []apiv1.ContextPointer{{Artifact: &p}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.ReadArtifact(context.Background(), p, 100)
		if got != nil {
			t.Fatal("partial bytes returned")
		}
		if errors.Is(failure, infra) {
			if !errors.Is(err, infra) || errors.Is(err, ErrInvalid) {
				t.Fatalf("infrastructure error misclassified: %v", err)
			}
		} else if !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad bytes not rejected: %v", err)
		}
	}
}
