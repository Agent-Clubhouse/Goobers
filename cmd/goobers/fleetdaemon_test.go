package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/fleet"
)

type blockingFleetConnector struct {
	started chan struct{}
	stopped chan struct{}
}

func (c *blockingFleetConnector) Run(ctx context.Context) error {
	close(c.started)
	<-ctx.Done()
	close(c.stopped)
	return ctx.Err()
}

func TestDaemonFleetConnectorIsOptional(t *testing.T) {
	store := &fleetMemoryStorage{}
	originalStorage := newFleetStorage
	newFleetStorage = func() (fleet.Storage, error) { return store, nil }
	t.Cleanup(func() { newFleetStorage = originalStorage })

	done, started, err := startDaemonFleetConnector(context.Background(), "root")
	if err != nil || started || done != nil {
		t.Fatalf("done=%v started=%v err=%v", done, started, err)
	}
}

func TestDaemonFleetConnectorStopsOnCancellation(t *testing.T) {
	store := &fleetMemoryStorage{
		saved: true,
		record: fleet.Record{
			Association: fleet.Association{
				ProtocolVersion: fleet.ProtocolVersion,
			},
		},
	}
	blocking := &blockingFleetConnector{
		started: make(chan struct{}),
		stopped: make(chan struct{}),
	}
	originalStorage := newFleetStorage
	originalConnector := newDaemonFleetConnector
	newFleetStorage = func() (fleet.Storage, error) { return store, nil }
	newDaemonFleetConnector = func(fleet.Storage, string) interface{ Run(context.Context) error } {
		return blocking
	}
	t.Cleanup(func() {
		newFleetStorage = originalStorage
		newDaemonFleetConnector = originalConnector
	})

	ctx, cancel := context.WithCancel(context.Background())
	done, started, err := startDaemonFleetConnector(ctx, "root")
	if err != nil || !started {
		t.Fatalf("started=%v err=%v", started, err)
	}
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("connector did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("connector error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("connector did not stop")
	}
	select {
	case <-blocking.stopped:
	default:
		t.Fatal("connector did not observe cancellation")
	}
}
