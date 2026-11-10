package terminalwriter

import (
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/providerconfig"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/providers"
)

// InstanceBinding binds the applied configuration's gaggle whose backlog lives in
// repo, with no run involved. It is for reads that outlive any run (verifying a
// recorded block's items are still open): a run's pin is swept some days after it
// ends, after which Bind refuses it, so a record's own run cannot be the only way
// to reach its repository. The binding carries no RunID and is never used to
// write.
func InstanceBinding(set *instance.ConfigSet, digest string, repo providers.RepositoryRef) (Binding, bool) {
	if set == nil {
		return Binding{}, false
	}
	for i := range set.Gaggles {
		name := set.Gaggles[i].Name
		project := runtimewiring.GaggleProjectRef(set, name)
		if project.Owner == "" || project.Name == "" {
			continue
		}
		workspace := repositoryRef(project)
		workItems, err := providerconfig.ApplyBacklogProject(set, name, workspace)
		if err != nil || !sameRepository(workItems, repo) {
			continue
		}
		return Binding{Gaggle: name, Generation: digest, Project: project, Workspace: workspace, WorkItems: workItems}, true
	}
	return Binding{}, false
}
