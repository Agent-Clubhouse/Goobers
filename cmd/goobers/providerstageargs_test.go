package main

import (
	"flag"
	"testing"
)

func TestProviderStageRootArg(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantRoot  string
		wantOK    bool
		wantUsage bool
	}{
		{name: "default", wantRoot: ".", wantOK: true},
		{name: "explicit", args: []string{"instance"}, wantRoot: "instance", wantOK: true},
		{name: "too many", args: []string{"one", "two"}, wantOK: false, wantUsage: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fs := flag.NewFlagSet(test.name, flag.ContinueOnError)
			usageCalled := false
			fs.Usage = func() { usageCalled = true }
			if err := fs.Parse(test.args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			root, ok := providerStageRootArg(fs)
			if root != test.wantRoot || ok != test.wantOK || usageCalled != test.wantUsage {
				t.Fatalf("providerStageRootArg() = (%q, %t, usage=%t), want (%q, %t, usage=%t)",
					root, ok, usageCalled, test.wantRoot, test.wantOK, test.wantUsage)
			}
		})
	}
}
