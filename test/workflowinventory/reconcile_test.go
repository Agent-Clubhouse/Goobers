package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

var blockerReference = regexp.MustCompile(`#[1-9][0-9]*`)

func reconcileDormantBlockers(workflows []workflow, recorded map[string]row, resolve func(string) (bool, bool)) []string {
	var problems []string
	for _, current := range workflows {
		if current.Status != dormantStatus {
			continue
		}
		refs := blockerReference.FindAllString(recorded[current.File].BlockedOn, -1)
		if len(refs) == 0 {
			problems = append(problems, fmt.Sprintf("%s: dormant blocker has no resolvable tracking issue", current.File))
			continue
		}
		allClosed, allKnown := true, true
		for _, ref := range refs {
			closed, known := resolve(ref)
			if !known {
				allKnown = false
				problems = append(problems, fmt.Sprintf("%s: cannot resolve dormant blocker %s", current.File, ref))
			}
			allClosed = allClosed && closed
		}
		if allKnown && allClosed {
			problems = append(problems, fmt.Sprintf("%s: every referenced dormant blocker (%s) is closed; verify whether to enable, retire, or re-scope this workflow", current.File, strings.Join(refs, ", ")))
		}
	}
	return problems
}

func TestDormantBlockerReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name, blockers string
		states         map[string]bool
		want           string
	}{
		{"live", "#1", map[string]bool{"#1": false}, ""},
		{"closed", "#1", map[string]bool{"#1": true}, "every referenced"},
		{"historical secondary", "#1 (redirected from #2)", map[string]bool{"#1": false, "#2": true}, ""},
		{"unknown", "#1", map[string]bool{}, "cannot resolve"},
		{"untracked", "operator review", nil, "no resolvable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := reconcileDormantBlockers([]workflow{{File: "dormant.yml", Status: dormantStatus}}, map[string]row{"dormant.yml": {BlockedOn: tc.blockers}}, func(ref string) (bool, bool) { state, known := tc.states[ref]; return state, known })
			if tc.want == "" && len(problems) != 0 {
				t.Fatalf("unexpected: %v", problems)
			}
			if tc.want != "" && !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Fatalf("problems=%v want=%s", problems, tc.want)
			}
		})
	}
}
