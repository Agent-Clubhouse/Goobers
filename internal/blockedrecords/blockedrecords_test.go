package blockedrecords

import (
	"testing"

	"github.com/goobers/goobers/providers"
)

var repo = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

func TestLookupIDStripsThePullRequestClaimPrefix(t *testing.T) {
	if got := LookupID("pr/955"); got != "955" {
		t.Fatalf("LookupID(pr/955) = %q", got)
	}
	if got := LookupID("955"); got != "955" {
		t.Fatalf("LookupID(955) = %q", got)
	}
}

func TestKeyIsRepositoryScoped(t *testing.T) {
	other := repo
	other.Name = "api"
	if Key(repo, "1") == Key(other, "1") || Key(repo, "1") == Key(repo, "2") {
		t.Fatal("keys collide across repositories or items")
	}
}

func TestFindCycleSeesATwoItemCycleAndQuarantinesUnscopedRecords(t *testing.T) {
	recs := map[string]Record{
		Key(repo, "1"): {Repository: repo, ItemID: "1", Blockers: []string{"2"}},
		Key(repo, "2"): {Repository: repo, ItemID: "2", Blockers: []string{"pr/1"}},
		"legacy":       {ItemID: "9", Blockers: []string{"1"}}, // no repository: never part of a graph
	}
	cycle := FindCycle(recs, Key(repo, "2"))
	if len(cycle.Affected) != 2 {
		t.Fatalf("cycle members = %+v, want 2", cycle.Affected)
	}
	if got := FindCycle(recs, "legacy"); len(got.Affected) != 0 {
		t.Fatalf("an unscoped record seeded a cycle: %+v", got)
	}
	if all := FindAllCycles(recs); len(all) != 1 {
		t.Fatalf("FindAllCycles = %d, want 1", len(all))
	}
	if !AppliesToRepository(recs[Key(repo, "1")], repo) || AppliesToRepository(recs["legacy"], repo) {
		t.Fatal("AppliesToRepository misclassifies")
	}
}
