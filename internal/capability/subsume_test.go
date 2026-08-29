package capability

import "testing"

func TestSubsumesIsReflexiveForEveryCanonicalCapability(t *testing.T) {
	for _, c := range All() {
		if !Subsumes(c, c) {
			t.Errorf("Subsumes(%q, %q) = false, want true (every capability satisfies itself)", c, c)
		}
	}
}

func TestIssuesWriteSubsumesIssuesRead(t *testing.T) {
	if !Subsumes(GitHubIssuesWrite, GitHubIssuesRead) {
		t.Fatalf("Subsumes(%q, %q) = false, want true (the #2386 breakage class: a broader write grant must satisfy a narrowed read requirement)", GitHubIssuesWrite, GitHubIssuesRead)
	}
}

// The relation must be directed: a read grant satisfying a write requirement
// would be a privilege escalation, exactly what ADR 0002's no-implicit-alias
// rule exists to prevent.
func TestIssuesReadDoesNotSubsumeIssuesWrite(t *testing.T) {
	if Subsumes(GitHubIssuesRead, GitHubIssuesWrite) {
		t.Fatalf("Subsumes(%q, %q) = true, want false (subsumption must never run narrow-to-broad)", GitHubIssuesRead, GitHubIssuesWrite)
	}
}

// Exhaustively pin the whole relation over All()×All(): reflexivity plus the
// single declared write⊇read pair, and nothing else. Any table addition must
// consciously edit this test, so a new pair is a reviewed decision rather
// than a drive-by (the same posture the registry itself takes for #74).
func TestSubsumesHoldsForExactlyTheDeclaredRelation(t *testing.T) {
	for _, held := range All() {
		for _, required := range All() {
			want := held == required ||
				(held == GitHubIssuesWrite && required == GitHubIssuesRead)
			if got := Subsumes(held, required); got != want {
				t.Errorf("Subsumes(%q, %q) = %t, want %t", held, required, got, want)
			}
		}
	}
}

// Pin the table's exact shape so additions are deliberate: one broader grant,
// one narrower capability under it, and every entry canonical, stage-
// declarable, and not a redundant self-mapping (reflexivity is Subsumes' job,
// not the table's).
func TestSubsumptionTableExactSizeAndHygiene(t *testing.T) {
	if len(subsumptions) != 1 {
		t.Fatalf("subsumptions has %d broader grants, want exactly 1 — additions must update this test deliberately", len(subsumptions))
	}
	pairs := 0
	for held, narrower := range subsumptions {
		if !Known(string(held)) {
			t.Errorf("subsumptions key %q is not a canonical capability", held)
		}
		if len(narrower) == 0 {
			t.Errorf("subsumptions key %q declares no narrower capabilities", held)
		}
		for _, required := range narrower {
			pairs++
			if !Known(string(required)) {
				t.Errorf("subsumptions[%q] lists non-canonical capability %q", held, required)
			}
			if required == held {
				t.Errorf("subsumptions[%q] redundantly lists itself", held)
			}
		}
	}
	if pairs != 1 {
		t.Fatalf("subsumptions declares %d non-reflexive pairs, want exactly 1 (github:issues:write ⊇ github:issues:read)", pairs)
	}
}
