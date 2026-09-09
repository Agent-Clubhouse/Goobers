package main

import (
	"errors"
	"io"
	"reflect"
	"testing"
)

type mutationPersistenceProbe struct {
	calls                       []string
	short                       bool
	writeErr, syncErr, closeErr error
}

func (p *mutationPersistenceProbe) Write(data []byte) (int, error) {
	p.calls = append(p.calls, "write")
	if p.short {
		return len(data) - 1, p.writeErr
	}
	return len(data), p.writeErr
}
func (p *mutationPersistenceProbe) Sync() error { p.calls = append(p.calls, "sync"); return p.syncErr }
func (p *mutationPersistenceProbe) Close() error {
	p.calls = append(p.calls, "close")
	return p.closeErr
}

func TestMutationSidecarPersistenceOrderingAndFailures(t *testing.T) {
	failure := errors.New("injected failure")
	for _, tc := range []struct {
		name                 string
		probe                mutationPersistenceProbe
		parentErr, errorWant error
		calls                []string
	}{
		{"durable", mutationPersistenceProbe{}, nil, nil, []string{"write", "sync", "close", "parent"}},
		{"short write", mutationPersistenceProbe{short: true}, nil, io.ErrShortWrite, []string{"write", "close"}},
		{"write failure", mutationPersistenceProbe{writeErr: failure}, nil, failure, []string{"write", "close"}},
		{"sync failure", mutationPersistenceProbe{syncErr: failure}, nil, failure, []string{"write", "sync", "close"}},
		{"close failure", mutationPersistenceProbe{closeErr: failure}, nil, failure, []string{"write", "sync", "close"}},
		{"directory failure", mutationPersistenceProbe{}, failure, failure, []string{"write", "sync", "close", "parent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.probe
			err := persistMutationSidecar(&p, []byte("receipt\n"), func() error { p.calls = append(p.calls, "parent"); return tc.parentErr })
			if !errors.Is(err, tc.errorWant) {
				t.Fatalf("error=%v want=%v", err, tc.errorWant)
			}
			if !reflect.DeepEqual(p.calls, tc.calls) {
				t.Fatalf("calls=%v want=%v", p.calls, tc.calls)
			}
		})
	}
}

func TestMutationSidecarPersistenceRetainsWriteAndCloseErrors(t *testing.T) {
	writeErr, closeErr := errors.New("write"), errors.New("close")
	p := &mutationPersistenceProbe{writeErr: writeErr, closeErr: closeErr}
	err := persistMutationSidecar(p, []byte("receipt\n"), func() error { t.Fatal("must not sync parent after failed receipt"); return nil })
	if !errors.Is(err, writeErr) || !errors.Is(err, closeErr) {
		t.Fatalf("lost error: %v", err)
	}
}
