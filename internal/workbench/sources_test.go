package workbench

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func sourceGaggle() apiv1.Gaggle {
	return apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "team"}, Spec: apiv1.GaggleSpec{
		Project:         apiv1.RepoRef{Provider: "github", Owner: "acme", Name: "code"},
		Backlog:         apiv1.BacklogRef{Provider: "github", Project: "acme/issues"},
		AdditionalRepos: []apiv1.RepoRef{{Provider: "ado", Owner: "acme", Project: "strategy", Name: "wiki", Branch: "published"}},
		Workbench: &apiv1.GaggleWorkbench{SchemaVersion: "sources/v1", RelationshipManifest: "links", Sources: []apiv1.WorkbenchSource{
			{Name: "backlog", Kind: "backlog", Objectives: &apiv1.WorkbenchObjectiveSelector{IDs: []string{"123"}, Types: []string{"Epic"}, Labels: []string{"objective"}}, Writes: &apiv1.WorkbenchWrites{Fields: []apiv1.WorkbenchField{"title"}, Relationships: []apiv1.WorkbenchRelationship{"parent-of"}}},
			{Name: "strategy", Kind: "documents", Repository: &apiv1.InteractiveRepositoryIdentity{Provider: "ado", Owner: "acme", Project: "strategy", Name: "wiki"}, Paths: []string{"objectives/payments.md"}, Writes: &apiv1.WorkbenchWrites{Fields: []apiv1.WorkbenchField{"title", "description"}, Relationships: []apiv1.WorkbenchRelationship{"contributes-to"}}},
			{Name: "links", Kind: "relationships", Repository: &apiv1.InteractiveRepositoryIdentity{Provider: "github", Owner: "acme", Name: "code"}, Paths: []string{"planning/links.yaml"}, Writes: &apiv1.WorkbenchWrites{Relationships: []apiv1.WorkbenchRelationship{"blocked-by"}}},
		}},
	}}
}

func TestBindSourcesPinsExistingTargetsWithoutGrantingAuthority(t *testing.T) {
	g := sourceGaggle()
	set, err := BindSources(g)
	if err != nil {
		t.Fatal(err)
	}
	if g.Spec.InteractiveAccess != nil || len(set.Sources) != 3 || set.Scope.GaggleID != "team" {
		t.Fatal("source declarations inferred interactive policy")
	}
	if got := set.Sources[0].BacklogIdentity; got.Provider != "github" || got.Owner != "acme" || got.Name != "issues" || got.Project != "" {
		t.Fatal(got)
	}
	if set.Sources[1].Repository.Branch != "published" || set.Sources[2].Repository.Branch != "main" || set.ManifestOwner.Path != "planning/links.yaml" {
		t.Fatal(set)
	}
	if !set.Sources[0].AllowsField("title") || set.Sources[0].AllowsField("state") || !set.Sources[2].AllowsRelationship("blocked-by") {
		t.Fatal("write allowlist widened")
	}
	set.Sources[0].Spec.Objectives.IDs[0] = "changed"
	set.Sources[0].Spec.Objectives.Types[0] = "changed"
	set.Sources[0].Spec.Objectives.Labels[0] = "changed"
	set.Sources[0].Spec.Writes.Fields[0] = "state"
	set.Sources[0].Spec.Writes.Relationships[0] = "references"
	set.Sources[1].Spec.Paths[0] = "changed.md"
	set.Sources[1].Spec.Repository.Name = "changed"
	if g.Spec.Workbench.Sources[0].Objectives.IDs[0] != "123" || g.Spec.Workbench.Sources[0].Objectives.Types[0] != "Epic" || g.Spec.Workbench.Sources[0].Objectives.Labels[0] != "objective" || g.Spec.Workbench.Sources[0].Writes.Fields[0] != "title" || g.Spec.Workbench.Sources[0].Writes.Relationships[0] != "parent-of" || g.Spec.Workbench.Sources[1].Paths[0] != "objectives/payments.md" || g.Spec.Workbench.Sources[1].Repository.Name != "wiki" {
		t.Fatal("bound sources alias configuration")
	}
}

func TestBindSourcesRejectsAmbiguousAndForeignTargets(t *testing.T) {
	cases := map[string]func(*apiv1.Gaggle){
		"version":           func(g *apiv1.Gaggle) { g.Spec.Workbench.SchemaVersion = "other" },
		"empty":             func(g *apiv1.Gaggle) { g.Spec.Workbench.Sources = nil },
		"binding duplicate": func(g *apiv1.Gaggle) { g.Spec.Workbench.Sources[1].Name = "backlog" },
		"foreign repo":      func(g *apiv1.Gaggle) { g.Spec.Workbench.Sources[1].Repository.Name = "foreign" },
		"ambiguous repo": func(g *apiv1.Gaggle) {
			g.Spec.AdditionalRepos = append(g.Spec.AdditionalRepos, g.Spec.AdditionalRepos[0])
		},
		"repo endpoint":    func(g *apiv1.Gaggle) { g.Spec.AdditionalRepos[0].BaseURL = "https://other.example" },
		"backlog endpoint": func(g *apiv1.Gaggle) { g.Spec.Backlog.BaseURL = "https://other.example" },
		"backlog target":   func(g *apiv1.Gaggle) { g.Spec.Backlog.Project = "acme/issues/more" },
		"backlog path":     func(g *apiv1.Gaggle) { g.Spec.Workbench.Sources[0].Paths = []string{"x.md"} },
		"backlog repo":     func(g *apiv1.Gaggle) { g.Spec.Workbench.Sources[0].Repository = g.Spec.Workbench.Sources[1].Repository },
		"backlog duplicate owner": func(g *apiv1.Gaggle) {
			s := g.Spec.Workbench.Sources[0]
			s.Name = "second"
			g.Spec.Workbench.Sources = append(g.Spec.Workbench.Sources, s)
		},
		"file duplicate owner": func(g *apiv1.Gaggle) {
			s := g.Spec.Workbench.Sources[1]
			s.Name = "second"
			g.Spec.Workbench.Sources = append(g.Spec.Workbench.Sources, s)
		},
		"unknown kind":    func(g *apiv1.Gaggle) { g.Spec.Workbench.Sources[1].Kind = "remote" },
		"traversal":       func(g *apiv1.Gaggle) { g.Spec.Workbench.Sources[1].Paths = []string{"../objective.md"} },
		"glob":            func(g *apiv1.Gaggle) { g.Spec.Workbench.Sources[1].Paths = []string{"objectives/*.md"} },
		"wrong extension": func(g *apiv1.Gaggle) { g.Spec.Workbench.Sources[1].Paths = []string{"objective.html"} },
		"multi manifest": func(g *apiv1.Gaggle) {
			g.Spec.Workbench.Sources[2].Paths = append(g.Spec.Workbench.Sources[2].Paths, "second.yml")
		},
		"missing manifest":   func(g *apiv1.Gaggle) { g.Spec.Workbench.RelationshipManifest = "unknown" },
		"wrong manifest":     func(g *apiv1.Gaggle) { g.Spec.Workbench.RelationshipManifest = "strategy" },
		"empty selector":     func(g *apiv1.Gaggle) { g.Spec.Workbench.Sources[0].Objectives = &apiv1.WorkbenchObjectiveSelector{} },
		"duplicate selector": func(g *apiv1.Gaggle) { g.Spec.Workbench.Sources[0].Objectives.IDs = []string{"123", "123"} },
		"document selector":  func(g *apiv1.Gaggle) { g.Spec.Workbench.Sources[1].Objectives = g.Spec.Workbench.Sources[0].Objectives },
		"unknown write": func(g *apiv1.Gaggle) {
			g.Spec.Workbench.Sources[0].Writes.Fields = []apiv1.WorkbenchField{"credentials"}
		},
		"document state": func(g *apiv1.Gaggle) { g.Spec.Workbench.Sources[1].Writes.Fields = []apiv1.WorkbenchField{"state"} },
		"manifest field": func(g *apiv1.Gaggle) { g.Spec.Workbench.Sources[2].Writes.Fields = []apiv1.WorkbenchField{"title"} },
		"document hierarchy": func(g *apiv1.Gaggle) {
			g.Spec.Workbench.Sources[1].Writes.Relationships = []apiv1.WorkbenchRelationship{"parent-of"}
		},
		"derived relation": func(g *apiv1.Gaggle) {
			g.Spec.Workbench.Sources[0].Writes.Relationships = []apiv1.WorkbenchRelationship{"observed-in-run"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			g := sourceGaggle()
			mutate(&g)
			if _, err := BindSources(g); err == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
}

func TestBindSourcesADOAndAbsentConfiguration(t *testing.T) {
	g := sourceGaggle()
	g.Spec.Workbench = nil
	set, err := BindSources(g)
	if err != nil || len(set.Sources) != 0 || set.ManifestOwner != nil {
		t.Fatal(set, err)
	}
	g = sourceGaggle()
	g.Spec.Workbench.Sources = g.Spec.Workbench.Sources[:1]
	g.Spec.Workbench.RelationshipManifest = ""
	g.Spec.Backlog = apiv1.BacklogRef{Provider: "ado", Project: "boards"}
	if _, err = BindSources(g); err == nil {
		t.Fatal("GitHub owner used as ADO organization")
	}
	g.Spec.Project = apiv1.RepoRef{Provider: "ado", Owner: "org", Project: "code-project", Name: "code"}
	set, err = BindSources(g)
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Sources[0].BacklogIdentity; got.Owner != "org" || got.Project != "boards" || got.Name != "" {
		t.Fatal(got)
	}
}
