package main

import (
	"errors"
	"path/filepath"

	"github.com/goobers/goobers/internal/cancelreceipt"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func newPersistentDaemonCancelService(layout instance.Layout, runners *daemonRunnerRegistry, log *journal.InstanceLog) (*daemonCancelService, error) {
	if log == nil {
		return nil, errors.New("cancellation requires an audit journal")
	}
	path, err := filepath.Abs(filepath.Join(layout.SchedulerDir(), "cancellation-receipts.db"))
	if err != nil {
		return nil, err
	}
	receipts, err := cancelreceipt.Open(path)
	if err != nil {
		return nil, err
	}
	s := newDaemonCancelService(runners)
	s.auditLog, s.receipts = log, receipts
	return s, nil
}
