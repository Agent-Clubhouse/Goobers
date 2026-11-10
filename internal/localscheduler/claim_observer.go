package localscheduler

import (
	"path/filepath"
	"sync"
)

var claimMutationObservers sync.Map

type claimMutationObserver struct {
	notify func([]ClaimEntry)
}

// RegisterClaimMutationObserver installs the process-local observer for path.
// Every successful durable ledger mutation reports the complete live snapshot.
func RegisterClaimMutationObserver(path string, observer func([]ClaimEntry)) func() {
	key := filepath.Clean(path)
	registration := &claimMutationObserver{notify: observer}
	claimMutationObservers.Store(key, registration)
	return func() {
		claimMutationObservers.CompareAndDelete(key, registration)
	}
}

func notifyClaimMutation(path string, entries []ClaimEntry) {
	observer, ok := claimMutationObservers.Load(filepath.Clean(path))
	if !ok {
		return
	}
	observer.(*claimMutationObserver).notify(entries)
}
