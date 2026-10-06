package readservice

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/decisiongate"
)

const (
	faultDomainQuestionName       = "backprop-fault-domain"
	attributorQualityQuestionName = "backprop-attributor-quality"
)

var faultDomainQuestion = decider.Choice(
	"Classify only the supplied attributor prose by the component it says caused the failure. Choose mixed-or-unknown when the prose is ambiguous or lacks a causal claim.",
	map[string]any{
		string(creditgraph.FaultDomainProductRuntime): "Goobers scheduler, daemon, worktree, journal, claim, admission, publication, or recovery behavior",
		string(creditgraph.FaultDomainWorkflow):       "workflow definition, routing, topology, instructions, interpretation, or tool selection",
		string(creditgraph.FaultDomainExternal):       "agent harness, model service, provider, credentials, network, operating system, or environment outside Goobers",
		string(creditgraph.FaultDomainUnknown):        "mixed, ambiguous, or insufficient causal prose",
	},
)

var attributorQualityQuestion = decider.Score(
	"Score how specifically the supplied prose identifies a causal component, rather than merely naming symptoms or possible domains.",
	[]any{
		"no supported causal claim",
		"vague or substantially ambiguous",
		"specific cause with some ambiguity",
		"specific, actionable, evidence-linked cause",
	},
)

type faultDomainAdvisor struct {
	gate *decisiongate.Gate
}

func (a faultDomainAdvisor) ClassifyFaultDomain(ctx context.Context, text string) (creditgraph.FaultDomainAdvice, error) {
	if a.gate == nil {
		return creditgraph.FaultDomainAdvice{}, fmt.Errorf("decisiongate: fault-domain gate is required")
	}
	state := struct {
		AttributorText string `json:"attributorText"`
	}{AttributorText: text}
	domain, err := a.gate.JudgeChoice(ctx, faultDomainQuestionName, state, faultDomainQuestion)
	if err != nil {
		return creditgraph.FaultDomainAdvice{}, err
	}
	advice := creditgraph.FaultDomainAdvice{
		Domain:     creditgraph.FaultDomain(domain.Choice),
		Confidence: domain.Confidence,
	}
	if domain.Choice == "" {
		advice.Domain = creditgraph.FaultDomainUnknown
	}
	quality, err := a.gate.JudgeScore(ctx, attributorQualityQuestionName, state, attributorQualityQuestion)
	if quality.Score != nil {
		normalized := *quality.Score / 3
		advice.Quality = &normalized
	}
	advice.QualityConfidence = quality.Confidence
	if err != nil {
		return advice, err
	}
	return advice, nil
}
