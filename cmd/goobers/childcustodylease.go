package main

import "sync"

// acquireChildCustody excludes execution/continuation owners while a bounded
// terminal snapshot or family settlement is observed. It is per run, so Git
// work never holds the global registry mutex or blocks unrelated runs. Entries
// live only for the operation and are removed on every deferred release.
func (r *daemonRunnerRegistry) acquireChildCustody(runID string) (func(), bool) {
	if r == nil {
		return func() {}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owners[runID].owner != nil || r.childCustody[runID] != nil {
		return func() {}, false
	}
	if r.childCustody == nil {
		r.childCustody = map[string]chan struct{}{}
	}
	done := make(chan struct{})
	r.childCustody[runID] = done
	var once sync.Once
	return func() { once.Do(func() { r.mu.Lock(); delete(r.childCustody, runID); close(done); r.mu.Unlock() }) }, true
}

// lockRunTracking returns with the registry mutex held. An intervention refuses
// overlapping custody; startup tracking waits for the bounded holder to finish.
func (r *daemonRunnerRegistry) lockRunTracking(runID string, compatible bool) bool {
	for {
		r.mu.Lock()
		done := r.childCustody[runID]
		if done == nil {
			return true
		}
		r.mu.Unlock()
		if compatible {
			return false
		}
		<-done
	}
}
