package blockedcycle

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/goobers/goobers/providers"
)

func testRecords(repo providers.RepositoryRef, dependencies map[string][]string) []Record {
	itemIDs := make([]string, 0, len(dependencies))
	for itemID := range dependencies {
		itemIDs = append(itemIDs, itemID)
	}
	sort.Strings(itemIDs)

	records := make([]Record, 0, len(itemIDs))
	for _, itemID := range itemIDs {
		records = append(records, Record{Repository: repo, ItemID: itemID, Blockers: dependencies[itemID]})
	}
	return records
}

func TestFind(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	tests := []struct {
		name         string
		dependencies map[string][]string
		item         string
		wantAffected []string
		wantPaths    [][]string
	}{
		{
			name:         "no cycle",
			dependencies: map[string][]string{"500": {"501"}},
			item:         "500",
		},
		{
			name:         "self loop",
			dependencies: map[string][]string{"500": {"500"}},
			item:         "500",
			wantAffected: []string{"500"},
			wantPaths:    [][]string{{"500", "500"}},
		},
		{
			name:         "two member cycle",
			dependencies: map[string][]string{"500": {"501"}, "501": {"500"}},
			item:         "500",
			wantAffected: []string{"500", "501"},
			wantPaths:    [][]string{{"500", "501", "500"}},
		},
		{
			name:         "three member cycle from any seed",
			dependencies: map[string][]string{"500": {"501"}, "501": {"502"}, "502": {"500"}},
			item:         "501",
			wantAffected: []string{"501", "500", "502"},
			wantPaths:    [][]string{{"501", "502", "500", "501"}},
		},
		{
			name:         "blocker without its own record cannot cycle",
			dependencies: map[string][]string{"500": {"501", "502"}, "502": {"503"}},
			item:         "500",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Find(testRecords(repo, tc.dependencies), Node{Repository: repo, ItemID: tc.item})
			var gotAffected []string
			for _, node := range got.Affected {
				gotAffected = append(gotAffected, node.ItemID)
			}
			if !reflect.DeepEqual(gotAffected, tc.wantAffected) {
				t.Errorf("affected = %v, want %v", gotAffected, tc.wantAffected)
			}
			if !reflect.DeepEqual(got.Paths, tc.wantPaths) {
				t.Errorf("paths = %v, want %v", got.Paths, tc.wantPaths)
			}
		})
	}
}

func TestFindRequiresRepositoryScope(t *testing.T) {
	if got := Find(nil, Node{ItemID: "500"}); len(got.Affected) != 0 {
		t.Fatalf("affected = %v, want no cycle for an unscoped node", got.Affected)
	}
}

func TestFindCapsRepresentativePaths(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	dependencies := make(map[string][]string)
	const nodes = 12
	for i := 0; i < nodes; i++ {
		itemID := fmt.Sprintf("%d", 500+i)
		for j := 0; j < nodes; j++ {
			dependencies[itemID] = append(dependencies[itemID], fmt.Sprintf("%d", 500+j))
		}
	}

	got := Find(testRecords(repo, dependencies), Node{Repository: repo, ItemID: "500"})
	if len(got.Affected) != nodes {
		t.Fatalf("affected = %d, want every one of the %d members", len(got.Affected), nodes)
	}
	if len(got.Paths) != MaxPaths || !got.MorePaths {
		t.Fatalf("paths = %v, more = %v; want %d capped paths and a more-paths notice", got.Paths, got.MorePaths, MaxPaths)
	}
}

func TestFindAllReportsEveryDisjointCycle(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	other := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}
	records := append(
		testRecords(repo, map[string][]string{"500": {"501"}, "501": {"500"}, "600": {"601"}}),
		testRecords(other, map[string][]string{"700": {"700"}})...,
	)

	cycles := FindAll(records)
	if len(cycles) != 2 {
		t.Fatalf("cycles = %d, want the two independent cycles", len(cycles))
	}

	var members [][]string
	for _, cycle := range cycles {
		var ids []string
		for _, node := range cycle.Affected {
			ids = append(ids, node.Repository.Name+"#"+node.ItemID)
		}
		members = append(members, ids)
	}
	want := [][]string{{"api#700"}, {"web#500", "web#501"}}
	if !reflect.DeepEqual(members, want) {
		t.Fatalf("cycle members = %v, want %v", members, want)
	}
}

func TestFindAllIgnoresAcyclicRecords(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	if cycles := FindAll(testRecords(repo, map[string][]string{"500": {"501"}, "501": {"502"}})); cycles != nil {
		t.Fatalf("cycles = %v, want none", cycles)
	}
}

func TestBuildGraphSkipsUnscopedRecordsAndDeduplicatesEdges(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	graph, reverseGraph := BuildGraph([]Record{
		{Repository: repo, ItemID: "500", Blockers: []string{"501", "501"}},
		{ItemID: "600", Blockers: []string{"601"}},
	})

	node := Node{Repository: repo, ItemID: "500"}
	blocker := Node{Repository: repo, ItemID: "501"}
	if want := []Node{blocker}; !reflect.DeepEqual(graph[node], want) {
		t.Errorf("graph[%v] = %v, want %v", node, graph[node], want)
	}
	if len(graph) != 1 {
		t.Errorf("graph = %v, want only the repository-scoped record", graph)
	}
	if want := []Node{node}; !reflect.DeepEqual(reverseGraph[blocker], want) {
		t.Errorf("reverseGraph[%v] = %v, want %v", blocker, reverseGraph[blocker], want)
	}
}

func TestRepositoryIdentity(t *testing.T) {
	tests := []struct {
		name string
		repo providers.RepositoryRef
		want string
	}{
		{name: "empty"},
		{
			name: "github",
			repo: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
			want: "github/acme/web",
		},
		{
			name: "project scoped and escaped",
			repo: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme corp", Project: "a/b", Name: "web"},
			want: "github/acme%20corp/a%2Fb/web",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepositoryIdentity(tc.repo); got != tc.want {
				t.Errorf("RepositoryIdentity = %q, want %q", got, tc.want)
			}
		})
	}
}
