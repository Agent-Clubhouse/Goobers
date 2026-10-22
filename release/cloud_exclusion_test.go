package main

import "testing"

func TestReleaseExcludesGoobersCloud(t *testing.T) {
	var found bool
	for _, binary := range releaseBinaries {
		if binary.name != "goobers-cloud" {
			continue
		}
		found = true
		if binary.buildPackage != "./cmd/goobers-cloud" {
			t.Fatalf("goobers-cloud package = %q", binary.buildPackage)
		}
		if binary.publish {
			t.Fatal("goobers-cloud must stay excluded from release artifacts until P3 exit")
		}
	}
	if !found {
		t.Fatal("goobers-cloud has no release-packaging registry entry")
	}
}
