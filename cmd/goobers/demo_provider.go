package main

import (
	"fmt"
	"io"
	"strings"
)

const demoProviderCommand = "__demo-provider"

var demoProviderInputKeys = []string{
	"itemID",
	"itemTitle",
	"pullNumber",
	"pullRequestURL",
	"headSHA",
	"baseSHA",
	"verdict",
}

func runDemoProvider(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		pf(stderr, "usage: goobers %s <curate|implement|review|merge-preview>\n", demoProviderCommand)
		return 2
	}
	phase := args[0]
	if !validDemoProviderPhase(phase) {
		pf(stderr, "error: unknown demo provider phase %q\n", phase)
		return 2
	}

	inputs := make(map[string]string, len(demoProviderInputKeys))
	for _, key := range demoProviderInputKeys {
		inputs[key] = providerInput(key, "")
	}
	payload, summary, err := demoProviderPayload(phase, inputs)
	if err != nil {
		pf(stderr, "error: mock provider %s: %v\n", phase, err)
		return 1
	}
	resultFile := providerInput("resultFile", phase+".json")
	if err := writeProviderStageResult(resultFile, payload); err != nil {
		pf(stderr, "error: mock provider %s: %v\n", phase, err)
		return 1
	}
	pf(stdout, "mock provider: %s\n", summary)
	return 0
}

func validDemoProviderPhase(phase string) bool {
	switch phase {
	case "curate", "implement", "review", "merge-preview":
		return true
	default:
		return false
	}
}

func demoProviderPayload(phase string, inputs map[string]string) (map[string]interface{}, string, error) {
	switch phase {
	case "curate":
		return map[string]interface{}{
			"provider":   "mock",
			"phase":      phase,
			"itemID":     "DEMO-1",
			"itemTitle":  "Clarify the first-run guide",
			"itemStatus": "ready",
		}, "curated DEMO-1 as ready", nil
	case "implement":
		itemID, err := requiredDemoProviderInput(inputs, "itemID")
		if err != nil {
			return nil, "", err
		}
		itemTitle, err := requiredDemoProviderInput(inputs, "itemTitle")
		if err != nil {
			return nil, "", err
		}
		return map[string]interface{}{
			"provider":       "mock",
			"phase":          phase,
			"itemID":         itemID,
			"itemTitle":      itemTitle,
			"pullNumber":     "1",
			"pullRequestURL": "mock://demo/pulls/1",
			"headSHA":        "demo-head-001",
			"baseSHA":        "demo-base-001",
		}, "implemented DEMO-1 as mock pull request #1", nil
	case "review":
		payload, err := demoPullRequestInputs(inputs)
		if err != nil {
			return nil, "", err
		}
		payload["provider"] = "mock"
		payload["phase"] = phase
		payload["verdict"] = "pass"
		payload["rationale"] = "fixture change is scoped and ready to merge"
		return payload, "reviewed mock pull request #1: pass", nil
	case "merge-preview":
		payload, err := demoPullRequestInputs(inputs)
		if err != nil {
			return nil, "", err
		}
		verdict, err := requiredDemoProviderInput(inputs, "verdict")
		if err != nil {
			return nil, "", err
		}
		if verdict != "pass" {
			return nil, "", fmt.Errorf("verdict must be pass, got %q", verdict)
		}
		payload["provider"] = "mock"
		payload["phase"] = phase
		payload["verdict"] = verdict
		payload["mergeMethod"] = "squash"
		payload["mergePreview"] = "would squash mock pull request #1 into main"
		payload["wouldMerge"] = true
		return payload, "merge preview ready for mock pull request #1 (no write performed)", nil
	default:
		return nil, "", fmt.Errorf("unknown phase %q", phase)
	}
}

func demoPullRequestInputs(inputs map[string]string) (map[string]interface{}, error) {
	payload := make(map[string]interface{}, 6)
	for _, key := range []string{"itemID", "pullNumber", "pullRequestURL", "headSHA", "baseSHA"} {
		value, err := requiredDemoProviderInput(inputs, key)
		if err != nil {
			return nil, err
		}
		payload[key] = value
	}
	return payload, nil
}

func requiredDemoProviderInput(inputs map[string]string, key string) (string, error) {
	value := strings.TrimSpace(inputs[key])
	if value == "" {
		return "", fmt.Errorf("%s input is required", key)
	}
	return value, nil
}
