package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type effectiveGoober struct {
	Name           string                     `json:"name"`
	Instructions   string                     `json:"instructions"`
	Skills         []string                   `json:"skills,omitempty"`
	Model          string                     `json:"model,omitempty"`
	Harness        string                     `json:"harness"`
	HarnessOptions map[string]json.RawMessage `json:"harnessOptions,omitempty"`
}

// ComputeGooberDigest returns the stable content identity of the resolved
// goobers that participate in def.
func ComputeGooberDigest(def Definition, goobers map[string]apiv1.GooberSpec, instructions map[string]string) (string, error) {
	names := participatingGoobers(def)
	effective := make([]effectiveGoober, 0, len(names))
	for _, name := range names {
		spec, ok := goobers[name]
		if !ok {
			return "", fmt.Errorf("participating goober %q is not defined", name)
		}
		content, ok := instructions[name]
		if !ok {
			return "", fmt.Errorf("participating goober %q has no resolved instructions", name)
		}
		harness := spec.Harness
		if harness == "" {
			harness = apiv1.HarnessCopilot
		}
		options := make(map[string]json.RawMessage, len(spec.HarnessOptions))
		for key, value := range spec.HarnessOptions {
			if !json.Valid(value.Raw) {
				return "", fmt.Errorf("participating goober %q harness option %q is not valid JSON", name, key)
			}
			options[key] = append(json.RawMessage(nil), value.Raw...)
		}
		if len(options) == 0 {
			options = nil
		}
		effective = append(effective, effectiveGoober{
			Name:           name,
			Instructions:   content,
			Skills:         canonicalSet(spec.Skills),
			Model:          spec.Model,
			Harness:        string(harness),
			HarnessOptions: options,
		})
	}
	return canonicalDigest(effective)
}

func participatingGoobers(def Definition) []string {
	names := map[string]struct{}{}
	for _, task := range def.Spec.Tasks {
		if task.Type == apiv1.TaskAgentic && task.Goober != "" {
			names[task.Goober] = struct{}{}
		}
	}
	for _, gate := range def.Spec.Gates {
		if gate.Evaluator == apiv1.EvaluatorAgentic && gate.Agentic != nil && gate.Agentic.Goober != "" {
			names[gate.Agentic.Goober] = struct{}{}
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func canonicalSet(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func canonicalDigest(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
