package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/fleet"
	"github.com/goobers/goobers/internal/version"
)

var newDaemonFleetConnector = func(storage fleet.Storage, root string) interface{ Run(context.Context) error } {
	return fleet.NewConnector(storage, root, version.Get().Version)
}

func startDaemonFleetConnector(ctx context.Context, root string) (<-chan error, bool, error) {
	storage, err := newFleetStorage()
	if err != nil {
		return nil, false, err
	}
	record, err := storage.Load(root)
	if errors.Is(err, fleet.ErrNotAssociated) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if record.Association.Revoked {
		return nil, false, nil
	}
	done := make(chan error, 1)
	connector := newDaemonFleetConnector(storage, root)
	go func() {
		done <- connector.Run(ctx)
	}()
	return done, true, nil
}
