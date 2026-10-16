package main

import (
	"testing"

	"github.com/goobers/goobers/internal/runner"
)

func TestChildCustodyExcludesLiveRunsAndInterventionOwners(t *testing.T) {
	r := newDaemonRunnerRegistry()
	owner := &runner.Runner{}
	untrack := r.Track("child", "generated", owner)
	if release, ok := r.acquireChildCustody("child"); ok {
		release()
		t.Fatal("captured live child")
	}
	untrack()
	release, ok := r.acquireChildCustody("child")
	if !ok {
		t.Fatal("joined child not capturable")
	}
	if untrack, ok := r.TrackCompatible("child", owner); ok {
		untrack()
		t.Fatal("intervention entered captured workspace")
	}
	if release2, ok := r.acquireChildCustody("child"); ok {
		release2()
		t.Fatal("two concurrent captures")
	}
	release()
	release()
	untrack, ok = r.TrackCompatible("child", owner)
	if !ok {
		t.Fatal("custody was not released")
	}
	untrack()
	if len(r.childCustody) != 0 {
		t.Fatal("custody metadata accumulated")
	}
}
