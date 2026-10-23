package main

import (
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
)

func workerExecutionBearer(emitter *livejournal.HTTPEmitter, runID string) (string, error) {
	if emitter.Token != "" {
		return emitter.Token, nil
	}
	if emitter.Minter == nil {
		return "", nil
	}
	minter, ok := emitter.Minter.(interface {
		MintScoped(string, time.Duration, ...string) (string, error)
	})
	if !ok {
		return "", fmt.Errorf("worker shared execution requires a run-scoped bearer")
	}
	return minter.MintScoped(runID, 5*time.Minute, podauth.ScopeClaims)
}
