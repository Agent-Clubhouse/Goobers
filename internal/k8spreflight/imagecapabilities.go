package k8spreflight

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// #3967 corrected the latched memory admission gate first shipped in #3961.
// A manifest setting the knob against an older source revision is not proof
// of a working gate. Keep the incident's minimum, not a release-date guess.
const memoryGateMinimumCommit = "2bccb3ac094f578ae95547348d0d2bd0b2746c76"

func probeImageSourceRequirement(ctx context.Context, run overlayCommandRunner, required imageRequirements) error {
	if required.MinimumCommit != memoryGateMinimumCommit {
		return fmt.Errorf("unrecognized image source capability")
	}
	data, err := run(ctx, overlayCommand{Program: "gh", Args: []string{"api", "repos/Agent-Clubhouse/Goobers/compare/" + required.MinimumCommit + "..." + required.Commit, "--hostname", "github.com", "--jq", "{status: .status}"}})
	if err != nil {
		return fmt.Errorf("cannot establish memory-gate source ancestry: %w", err)
	}
	var comparison struct{ Status string }
	if err := json.Unmarshal(data, &comparison); err != nil {
		return fmt.Errorf("cannot decode source ancestry: %w", err)
	}
	if comparison.Status != "ahead" && comparison.Status != "identical" {
		return fmt.Errorf("GOOBERS_MEMORY_HIGH_WATER requires corrected memory gate at %s; pin %s ancestry=%q", required.MinimumCommit, required.Commit, comparison.Status)
	}
	return nil
}

func probeImageScriptVariables(invoke imageInvocation, imageOS string, variables []string) error {
	for _, variable := range variables {
		if variable != "GOCACHE_ORPHAN_ROOT" && variable != "GOCACHE_ORPHAN_AGE_MIN" {
			return fmt.Errorf("unrecognized cache-trimmer variable")
		}
		if imageOS != "linux" {
			return fmt.Errorf("cache-trimmer script capability on %s is unverified", imageOS)
		}
		_, err := invoke("/bin/sh", []string{"-c", `grep -q -F -- "$1" /usr/local/bin/gocache-trim`, "probe", variable}, nil)
		if err != nil {
			return fmt.Errorf("gocache-trim does not contain configured %s: %w", variable, err)
		}
	}
	return nil
}

func collectContainerRequirements(container *yaml.Node, required *imageRequirements) error {
	var tokens []string
	for _, field := range []string{"command", "args"} {
		args := nodeField(container, field)
		if args == nil {
			continue
		}
		if args.Kind != yaml.SequenceNode {
			return fmt.Errorf("container %s must be a sequence", field)
		}
		for index, arg := range args.Content {
			if arg.Kind != yaml.ScalarNode || arg.Tag != "!!str" {
				return fmt.Errorf("container %s must contain strings", field)
			}
			tokens = append(tokens, arg.Value)
			if strings.HasPrefix(arg.Value, "/usr/local/bin/") || field == "command" && index == 0 && absoluteImageExecutable(arg.Value) {
				required.Executables = append(required.Executables, arg.Value)
			} else if field == "command" && index == 0 && imageToolPattern.MatchString(arg.Value) {
				required.Tools = append(required.Tools, arg.Value)
			}
		}
	}
	return collectImageEnvironment(container, tokens, required)
}

func absoluteImageExecutable(value string) bool {
	return strings.HasPrefix(value, "/") || len(value) >= 3 && value[1] == ':' && (value[2] == '\\' || value[2] == '/')
}

func collectImageEnvironment(container *yaml.Node, tokens []string, required *imageRequirements) error {
	env := nodeField(container, "env")
	if env == nil {
		return nil
	}
	if env.Kind != yaml.SequenceNode {
		return fmt.Errorf("container env must be a sequence")
	}
	for _, entry := range env.Content {
		if entry.Kind != yaml.MappingNode {
			return fmt.Errorf("container env entries must be mappings")
		}
		name, value := nodeValue(entry, "name"), nodeValue(entry, "value")
		switch name {
		case "GOOBERS_MEMORY_HIGH_WATER":
			if value != "off" && (value != "" || nodeField(entry, "valueFrom") != nil) {
				required.MinimumCommit = memoryGateMinimumCommit
			}
		case "GOCACHE_ORPHAN_ROOT", "GOCACHE_ORPHAN_AGE_MIN":
			for _, token := range tokens {
				if token == "/usr/local/bin/gocache-trim" {
					required.ScriptVariables = append(required.ScriptVariables, name)
					break
				}
			}
		}
	}
	return nil
}
