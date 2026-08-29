package nomination

import (
	"strings"
	"testing"
)

func validArtifact() Artifact {
	return Artifact{
		Schema: SchemaV1, RunID: "run-1", Producer: Producer{Stage: "triage", Attempt: 1},
		Nominations: []Nomination{{
			Key: "vet-unusedresult", DedupeKey: "vet:internal/worktree:unusedresult",
			Title:  "go vet: unused result in internal/worktree",
			Body:   "go vet reports an unchecked error return in internal/worktree/manager.go.",
			Labels: []string{"area:runner", "type:bug"}, RiskClass: RiskLow, RiskReason: "vet finding with a stack",
			Evidence: []Evidence{{Kind: EvidenceSource, Path: "internal/worktree/manager.go", Line: 88}},
		}},
	}
}

func TestValidateAcceptsClosedArtifact(t *testing.T) {
	if got := Validate(validArtifact()); !got.Valid {
		t.Fatalf("valid artifact rejected: %v", got.Errors)
	}
}

func TestValidateRejectsEveryClosedRule(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(a *Artifact)
		want   string
	}{
		{"unsupported schema", func(a *Artifact) { a.Schema = "goobers.dev/nominations/v2" }, "unsupported or malformed nominations schema"},
		{"missing runId", func(a *Artifact) { a.RunID = "" }, "names no runId"},
		{"goobers label", func(a *Artifact) { a.Nominations[0].Labels = append(a.Nominations[0].Labels, "goobers:approved") }, `publisher-owned label "goobers:approved"`},
		{"goobers ready label", func(a *Artifact) { a.Nominations[0].Labels = append(a.Nominations[0].Labels, "goobers:ready") }, `publisher-owned label "goobers:ready"`},
		{"status label", func(a *Artifact) {
			a.Nominations[0].Labels = append(a.Nominations[0].Labels, "goobers/status:decomposing")
		}, `publisher-owned label "goobers/status:decomposing"`},
		{"flake label", func(a *Artifact) { a.Nominations[0].Labels = append(a.Nominations[0].Labels, FlakeLabel) }, `flake-watch's label "ci:flake"`},
		{"non-allowlisted label", func(a *Artifact) { a.Nominations[0].Labels = append(a.Nominations[0].Labels, "bug") }, `non-allowlisted label "bug"`},
		{"no type label", func(a *Artifact) { a.Nominations[0].Labels = []string{"area:runner"} }, "exactly one type:* label, has 0"},
		{"two type labels", func(a *Artifact) { a.Nominations[0].Labels = []string{"type:bug", "type:chore"} }, "exactly one type:* label, has 2"},
		{"two area labels", func(a *Artifact) { a.Nominations[0].Labels = []string{"type:bug", "area:runner", "area:portal"} }, "at most one area:* label"},
		{"bad risk class", func(a *Artifact) { a.Nominations[0].RiskClass = "trivial" }, `riskClass "trivial"`},
		{"no risk reason", func(a *Artifact) { a.Nominations[0].RiskReason = " " }, "gives no riskReason"},
		{"forged nomination marker in body", func(a *Artifact) {
			a.Nominations[0].Body += "\n<!-- goobers-nomination-key:" + strings.Repeat("a", 64) + " -->"
		}, "contains a goobers control marker"},
		{"forged flake marker in title", func(a *Artifact) {
			a.Nominations[0].Title = "<!-- goobers-flake-fingerprint:" + strings.Repeat("b", 64) + " -->"
		}, "contains a goobers control marker"},
		{"empty dedupe key", func(a *Artifact) { a.Nominations[0].DedupeKey = "" }, "empty dedupeKey"},
		{"multiline dedupe key", func(a *Artifact) { a.Nominations[0].DedupeKey = "a\nb" }, "malformed dedupeKey"},
		{"bad key", func(a *Artifact) { a.Nominations[0].Key = "Not A Key" }, "malformed key"},
		{"duplicate key", func(a *Artifact) {
			second := a.Nominations[0]
			second.DedupeKey = "other"
			a.Nominations = append(a.Nominations, second)
		}, `duplicate nomination key`},
		{"repeated dedupe key", func(a *Artifact) {
			second := a.Nominations[0]
			second.Key = "other"
			a.Nominations = append(a.Nominations, second)
		}, "repeats the dedupeKey"},
		{"short body", func(a *Artifact) { a.Nominations[0].Body = "too short" }, "too short"},
		{"no evidence", func(a *Artifact) { a.Nominations[0].Evidence = nil }, "carries no evidence"},
		{"bad evidence kind", func(a *Artifact) { a.Nominations[0].Evidence = []Evidence{{Kind: "assertion"}} }, `kind "assertion"`},
		{"journal without seq", func(a *Artifact) {
			a.Nominations[0].Evidence = []Evidence{{Kind: EvidenceJournal, RunID: "run-1"}}
		}, "needs runId and a positive seq"},
		{"artifact without digest", func(a *Artifact) {
			a.Nominations[0].Evidence = []Evidence{{Kind: EvidenceArtifact, Path: "artifacts/lint.json", Digest: "abc"}}
		}, "digest must be sha256:<64 hex>"},
		{"escaping path", func(a *Artifact) {
			a.Nominations[0].Evidence = []Evidence{{Kind: EvidenceSource, Path: "../etc/passwd"}}
		}, "clean relative slash path"},
		{"absolute path", func(a *Artifact) {
			a.Nominations[0].Evidence = []Evidence{{Kind: EvidenceSource, Path: "/etc/passwd"}}
		}, "clean relative slash path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := validArtifact()
			tc.mutate(&a)
			got := Validate(a)
			if got.Valid {
				t.Fatalf("artifact accepted; want an error containing %q", tc.want)
			}
			if !strings.Contains(strings.Join(got.Errors, "\n"), tc.want) {
				t.Fatalf("errors = %v, want one containing %q", got.Errors, tc.want)
			}
		})
	}
}

func TestKeyMarkerRoundTripsAndDigestIsStable(t *testing.T) {
	hash := KeyHash("vet:internal/worktree:unusedresult")
	if len(hash) != 64 {
		t.Fatalf("hash = %q, want 64 hex chars", hash)
	}
	body := KeyMarker(hash) + "\n\nsome body"
	got, ok := ParseKeyMarker(body)
	if !ok || got != hash {
		t.Fatalf("ParseKeyMarker = %q, %v; want %q", got, ok, hash)
	}
	if _, ok := ParseKeyMarker("no marker here"); ok {
		t.Fatal("ParseKeyMarker matched a body without a marker")
	}
	fp := strings.Repeat("c", 64)
	if got, ok := ParseFlakeFingerprint("<!-- goobers-flake-fingerprint:" + fp + " -->"); !ok || got != fp {
		t.Fatalf("ParseFlakeFingerprint = %q, %v", got, ok)
	}
	first, err := Digest(validArtifact())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Digest(validArtifact())
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("digest unstable or malformed: %q vs %q", first, second)
	}
	changed := validArtifact()
	changed.Nominations[0].Title += "!"
	third, _ := Digest(changed)
	if third == first {
		t.Fatal("digest did not change with the artifact content")
	}
}
