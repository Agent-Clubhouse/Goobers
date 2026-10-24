package main

import (
	"reflect"
	"testing"
)

func TestMergeReadinessChecksKeepsEveryReportedCheck(t *testing.T) {
	if got := mergeReadinessChecks(nil, nil); got != nil {
		t.Fatalf("no checks = %v, want nil", got)
	}
	got := mergeReadinessChecks(map[string]bool{"scheduler-gate:backlog": false}, map[string]bool{"terminalOutbox": true})
	want := map[string]bool{"scheduler-gate:backlog": false, "terminalOutbox": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged = %v, want %v", got, want)
	}
}
