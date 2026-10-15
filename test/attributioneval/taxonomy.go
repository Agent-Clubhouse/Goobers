package main

import "github.com/goobers/goobers/internal/creditgraph"

// mastCategory is the published multi-agent failure taxonomy Backprop uses as
// its label vocabulary: the three top-level categories of MAST ("Why Do
// Multi-Agent LLM Systems Fail?", arXiv:2503.13657), plus the two buckets
// MAST does not cover — failures outside the agent system (model, harness,
// provider, or environment) and an explicit unknown.
type mastCategory string

const (
	// mastSpecification covers failures rooted in how the task, roles, or
	// system were specified: weak instructions or a misconfigured model route.
	mastSpecification mastCategory = "specification"
	// mastInterAgentMisalignment covers failures in how agents coordinated or
	// how a decision matched its reasoning: a wrong tool choice or a declared
	// dependency on a subagent that never ran.
	mastInterAgentMisalignment mastCategory = "inter-agent-misalignment"
	// mastTaskVerification covers failures where good inputs were accepted or
	// interpreted without being verified correctly.
	mastTaskVerification mastCategory = "task-verification"
	// mastOutsideAgentSystem covers failures raised by a component outside
	// the agent system.
	mastOutsideAgentSystem mastCategory = "outside-agent-system"
	// mastUnknown is an explicit refusal to categorize.
	mastUnknown mastCategory = "unknown"
)

// taxonomyEntry maps one Backprop failure class onto the label vocabulary:
// its MAST category and the fault domain the class implies when the cause's
// evidence names no shared runtime or external component.
type taxonomyEntry struct {
	Class  creditgraph.FailureClass `json:"class"`
	MAST   mastCategory             `json:"mast"`
	Domain creditgraph.FaultDomain  `json:"domain"`
}

var failureClassMAST = map[creditgraph.FailureClass]mastCategory{
	creditgraph.ClassWeakInstructions:  mastSpecification,
	creditgraph.ClassRouting:           mastSpecification,
	creditgraph.ClassBadToolChoice:     mastInterAgentMisalignment,
	creditgraph.ClassTopology:          mastInterAgentMisalignment,
	creditgraph.ClassBadInterpretation: mastTaskVerification,
	creditgraph.ClassModel:             mastOutsideAgentSystem,
	creditgraph.ClassEnvironment:       mastOutsideAgentSystem,
	// A failed tool result alone does not say whether the agent misused the
	// tool or the tool's environment failed, so it stays uncategorized.
	creditgraph.ClassBadToolResult: mastUnknown,
	creditgraph.ClassUnknown:       mastUnknown,
}

// taxonomyOf maps one failure class onto the label vocabulary. A class the
// taxonomy does not know maps to unknown.
func taxonomyOf(class creditgraph.FailureClass) taxonomyEntry {
	category, ok := failureClassMAST[class]
	if !ok {
		category = mastUnknown
	}
	return taxonomyEntry{Class: class, MAST: category, Domain: creditgraph.ClassFaultDomain(class)}
}

func knownFailureClass(class creditgraph.FailureClass) bool {
	_, ok := failureClassMAST[class]
	return ok
}
