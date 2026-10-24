package runtimewiring

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// StageBacklogSettings is the part of a gaggle's backlog configuration an
// Azure DevOps stage applies to its provider: the backlog project its work
// item operations address (backlogRepoRefForStage), the claim-scope labels
// (stageADOBacklogScope) and the states that count as done for predecessor
// blocking (applyGaggleDoneStates). A stage pod has no
// instance config, so the worker resolves these from the gaggle and the
// dispatcher stamps them as dispatcher.GaggleBacklogEnv (#5957).
//
// Labels is the backlog's claim scope (#6098): on Azure DevOps the labels a
// work item must carry for this gaggle to select it from a project that other
// gaggles share. Missing it in a pod made the gaggle claim items meant for a
// sibling.
type StageBacklogSettings struct {
	Provider   apiv1.Provider           `json:"provider,omitempty"`
	Project    string                   `json:"project,omitempty"`
	Labels     []string                 `json:"labels,omitempty"`
	DoneStates *apiv1.BacklogDoneStates `json:"doneStates,omitempty"`
}

// ConfigSet renders the delivered settings as a one-gaggle config set, so a
// stage applies them through exactly the functions that apply the instance
// config (providerconfig.ApplyBacklogProject, GaggleADODoneStates).
func (s StageBacklogSettings) ConfigSet(gaggle string) *instance.ConfigSet {
	g := apiv1.Gaggle{Spec: apiv1.GaggleSpec{Backlog: apiv1.BacklogRef{Provider: s.Provider, Project: s.Project, Labels: s.Labels, DoneStates: s.DoneStates}}}
	g.Name = gaggle
	return &instance.ConfigSet{Gaggles: []apiv1.Gaggle{g}}
}
