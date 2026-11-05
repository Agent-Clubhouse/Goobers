package main

import (
	"testing"
)

func TestConfigDigestPublisherTracksTheLiveTree(t *testing.T) {
	// The publisher exists because the API handler is built before the config
	// reloader. A digest that stopped moving would make every worker look
	// converged forever — false assurance, worse than no alarm at all.
	p := newConfigDigestPublisher("sha256:first")
	if got := p.Get(); got != "sha256:first" {
		t.Errorf("Get() = %q", got)
	}
	p.Set("sha256:second")
	if got := p.Get(); got != "sha256:second" {
		t.Errorf("Get() after Set = %q, want the newly applied tree", got)
	}
	if got := (*configDigestPublisher)(nil).Get(); got != "" {
		t.Errorf("nil publisher Get() = %q, want empty", got)
	}
}
