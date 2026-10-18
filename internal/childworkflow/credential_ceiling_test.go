package childworkflow

import (
	"slices"
	"strings"
	"testing"
)

func TestProposalOwnsTrustedCredentialCeiling(t *testing.T) {
	p, err := validator(t, testContext()).Validate([]byte(validProposal))
	if err != nil {
		t.Fatal(err)
	}
	ceiling := p.CredentialCeiling()
	if ceiling.AllowPublication || !slices.Equal(ceiling.AllowedKeys, []string{"agent:model"}) {
		t.Fatalf("ceiling=%+v", ceiling)
	}
	ceiling.AllowedKeys[0] = "repo:push"
	if p.CredentialCeiling().AllowedKeys[0] != "agent:model" {
		t.Fatal("returned delegation aliases trusted source")
	}
}
func TestProposalRefusesInseparableCopilotModelAuthentication(t *testing.T) {
	input := testContext()
	g := input.Goobers["coder"]
	g.Harness = "copilot"
	input.Goobers["coder"] = g
	input.KnownHarnesses = append(input.KnownHarnesses, "copilot")
	source := strings.Replace(validProposal, "type: deterministic", "type: agentic\n      goober: coder\n      capabilities: [agent:model]", 1)
	source = strings.Replace(source, "      run:\n        command: [\"true\"]\n", "", 1)
	requireCode(t, validator(t, input), source, "credential_isolation")
	// An unused allowlisted goober has no access to this deterministic child.
	if _, err := validator(t, input).Validate([]byte(validProposal)); err != nil {
		t.Fatal(err)
	}
}
