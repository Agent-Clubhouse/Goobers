package localscheduler

import (
	"fmt"
	"sort"
	"strings"
)

// maxListedWorkflows bounds the catalog an UnknownWorkflowError spells out.
const maxListedWorkflows = 20

// UnknownWorkflowError reports a trigger target absent from the configured
// workflow catalog. Available is the catalog the caller may choose from,
// scoped to Gaggle when the target named a configured gaggle. UnknownGaggle
// reports that no configured workflow belongs to Gaggle; Available is then
// the whole catalog.
type UnknownWorkflowError struct {
	Gaggle        string
	Workflow      string
	UnknownGaggle bool
	Available     []WorkflowIdentity
}

func (e *UnknownWorkflowError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "no workflow named %q", e.Workflow)
	if e.Gaggle != "" {
		fmt.Fprintf(&b, " in gaggle %q", e.Gaggle)
	}
	if e.UnknownGaggle {
		fmt.Fprintf(&b, "; no gaggle named %q is configured", e.Gaggle)
	}
	if len(e.Available) == 0 {
		if !e.UnknownGaggle {
			b.WriteString("; no workflows are configured")
		}
		return b.String()
	}
	names := make([]string, 0, min(len(e.Available), maxListedWorkflows))
	for _, identity := range e.Available[:min(len(e.Available), maxListedWorkflows)] {
		if e.Gaggle != "" && !e.UnknownGaggle {
			names = append(names, identity.Workflow)
		} else {
			names = append(names, identity.Gaggle+"/"+identity.Workflow)
		}
	}
	b.WriteString("; available workflows: " + strings.Join(names, ", "))
	if extra := len(e.Available) - len(names); extra > 0 {
		fmt.Fprintf(&b, " (and %d more)", extra)
	}
	return b.String()
}

// WorkflowIdentities returns the scheduler's current workflow catalog in
// gaggle/workflow order. Like WorkflowCount it reads the map Reload replaces,
// so callers observe applied hot reloads.
func (s *Scheduler) WorkflowIdentities() []WorkflowIdentity {
	s.mu.Lock()
	identities := make([]WorkflowIdentity, 0, len(s.workflows))
	for identity := range s.workflows {
		identities = append(identities, identity)
	}
	s.mu.Unlock()
	sortWorkflowIdentities(identities)
	return identities
}

// EntryIdentities returns the identities entries would register with a
// scheduler, in gaggle/workflow order.
func EntryIdentities(entries []WorkflowEntry) []WorkflowIdentity {
	identities := make([]WorkflowIdentity, 0, len(entries))
	for _, entry := range entries {
		identities = append(identities, entryIdentity(entry))
	}
	sortWorkflowIdentities(identities)
	return identities
}

// ResolveWorkflowTarget applies the manual-trigger resolution rules to
// catalog without dispatching: a gaggle-qualified target must name a
// configured workflow in that gaggle, and an unqualified one must name
// exactly one configured workflow. It returns *UnknownWorkflowError when
// nothing matches and the scheduler's ambiguity refusal when an unqualified
// name matches several gaggles.
func ResolveWorkflowTarget(catalog []WorkflowIdentity, gaggle, workflow string) error {
	var available []WorkflowIdentity
	var gaggles []string
	for _, identity := range catalog {
		if gaggle != "" && identity.Gaggle != gaggle {
			continue
		}
		available = append(available, identity)
		if identity.Workflow == workflow {
			gaggles = append(gaggles, identity.Gaggle)
		}
	}
	switch {
	case len(gaggles) == 0:
		unknownGaggle := gaggle != "" && len(available) == 0
		if unknownGaggle {
			available = append([]WorkflowIdentity(nil), catalog...)
		}
		sortWorkflowIdentities(available)
		return &UnknownWorkflowError{Gaggle: gaggle, Workflow: workflow, UnknownGaggle: unknownGaggle, Available: available}
	case len(gaggles) > 1:
		return ambiguousWorkflowError(workflow, gaggles)
	}
	return nil
}

func ambiguousWorkflowError(workflow string, gaggles []string) error {
	gaggles = append([]string(nil), gaggles...)
	sort.Strings(gaggles)
	commands := make([]string, 0, len(gaggles))
	for _, gaggle := range gaggles {
		commands = append(commands, fmt.Sprintf("%q", "goobers run "+gaggle+"/"+workflow))
	}
	return fmt.Errorf(
		"localscheduler: workflow %q is ambiguous; candidate gaggles: %s; retry with %s",
		workflow, strings.Join(gaggles, ", "), strings.Join(commands, " or "),
	)
}

func sortWorkflowIdentities(identities []WorkflowIdentity) {
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].Gaggle != identities[j].Gaggle {
			return identities[i].Gaggle < identities[j].Gaggle
		}
		return identities[i].Workflow < identities[j].Workflow
	})
}
