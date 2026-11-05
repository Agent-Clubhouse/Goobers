package workerscenarios

import (
	"strings"
	"testing"
)

func TestRequirePassedEvidence(t *testing.T) {
	passed := `{"results":[
		{"id":"CHAOS-13","status":"passed"},
		{"id":"UPGRADE-11","status":"passed"}
	]}`
	if err := RequirePassedEvidence(
		strings.NewReader(passed), Chaos13, Upgrade11,
	); err != nil {
		t.Fatal(err)
	}
	for name, evidence := range map[string]string{
		"missing": `{"results":[{"id":"CHAOS-13","status":"passed"}]}`,
		"skipped": `{"results":[
			{"id":"CHAOS-13","status":"skipped"},
			{"id":"UPGRADE-11","status":"passed"}
		]}`,
		"failed": `{"results":[
			{"id":"CHAOS-13","status":"passed"},
			{"id":"UPGRADE-11","status":"failed"}
		]}`,
		"duplicate": `{"results":[
			{"id":"CHAOS-13","status":"passed"},
			{"id":"CHAOS-13","status":"passed"},
			{"id":"UPGRADE-11","status":"passed"}
		]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := RequirePassedEvidence(
				strings.NewReader(evidence), Chaos13, Upgrade11,
			); err == nil {
				t.Fatalf("%s evidence passed", name)
			}
		})
	}
}
