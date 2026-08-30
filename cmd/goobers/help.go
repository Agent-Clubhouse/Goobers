package main

import (
	"io"
	"strings"
)

const helpHelp = `Usage: goobers help [all|stages|COMMAND|CONCEPT]

List core commands with no topic, show the complete or workflow-stage command
list with all or stages, show a command's full help, or explain one of these concepts:
instance, gaggle, goober, workflow, stage, gate, harness, capability.
`

const workflowConcept = "A declarative, versioned step-machine describing triggers, stages, gates, retries, and run conditions."

var glossary = map[string]string{
	"instance":   "One Goobers installation and its runtime state: instance.yaml, validated config, scheduler journal, telemetry, managed workcopies, and gaggle run journals.",
	"gaggle":     "A team or bounded workforce: its project/backlog connections, goobers, and workflows.",
	"goober":     "An agent role or worker definition: instructions, harness, tools, skills, model options, and allowed capabilities.",
	"workflow":   workflowConcept,
	"stage":      "One unit of work. It is deterministic (a command or built-in operation) or agentic (a harness invocation with an explicit contract).",
	"gate":       "A decision state that branches a workflow using an automated check, agentic verdict, or human approval. A gate is not a stage.",
	"harness":    "The adapter that invokes an agentic stage's model and tools, such as GitHub Copilot CLI or Claude Code, behind the same invocation and result contract.",
	"capability": "A declared permission for a stage or goober to perform a specific operation. Undeclared capabilities fail closed and receive no credentials.",
	"manifest":   "The config source's manifest.yaml: the versioned entry point that declares which gaggle definitions the instance loads.",
	"tier":       "A deployment scale, not a product fork: solo and small-team tiers use the local runner, while cloud scale uses the conforming Temporal runner.",
}

var helpConceptTopics = []string{
	"instance",
	"gaggle",
	"goober",
	"workflow",
	"stage",
	"gate",
	"harness",
	"capability",
}

func runHelpCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stdout)
		return 0
	}
	if len(args) == 1 {
		switch args[0] {
		case "all":
			usageAll(stdout)
			return 0
		case "stages":
			usageStages(stdout)
			return 0
		case "-h", "--help", "help":
			pf(stdout, "%s", helpHelp)
			return 0
		}
	}

	topic := strings.Join(args, " ")
	if command, ok := helpCommand(topic); ok {
		pf(stdout, "%s", command.long)
		return 0
	}
	if len(args) == 1 {
		if prose, ok := glossary[topic]; ok && isHelpConcept(topic) {
			pf(stdout, "%s\n\n%s\n", topic, prose)
			return 0
		}
	}

	suggestion := closestHelpTopic(topic)
	pf(stderr, "goobers help: unknown topic %q; did you mean %q?\n", topic, suggestion)
	return 2
}

func isHelpConcept(topic string) bool {
	for _, candidate := range helpConceptTopics {
		if candidate == topic {
			return true
		}
	}
	return false
}

func helpCommand(topic string) (cliCommand, bool) {
	return findHelpCommand(cliCommands, nil, topic)
}

func findHelpCommand(commands []cliCommand, prefix []string, topic string) (cliCommand, bool) {
	for _, command := range commands {
		name := docDisplayName(command)
		if name == "" || isHiddenCompletionCommand(name) {
			continue
		}
		path := append(append([]string{}, prefix...), name)
		if strings.Join(path, " ") == topic && command.long != "" {
			return command, true
		}
		if subcommand, ok := findHelpCommand(command.subcommands, path, topic); ok {
			return subcommand, true
		}
	}
	return cliCommand{}, false
}

func helpTopics() []string {
	topics := []string{"all", "stages"}
	topics = append(topics, helpConceptTopics...)
	var collect func([]cliCommand, []string)
	collect = func(commands []cliCommand, prefix []string) {
		for _, command := range commands {
			name := docDisplayName(command)
			if name == "" || isHiddenCompletionCommand(name) {
				continue
			}
			path := append(append([]string{}, prefix...), name)
			if command.long != "" {
				topics = append(topics, strings.Join(path, " "))
			}
			collect(command.subcommands, path)
		}
	}
	collect(cliCommands, nil)
	return topics
}

func closestHelpTopic(topic string) string {
	best := ""
	bestDistance := -1
	for _, candidate := range helpTopics() {
		distance := helpEditDistance(topic, candidate)
		if bestDistance == -1 || distance < bestDistance {
			best = candidate
			bestDistance = distance
		}
	}
	return best
}

func helpEditDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current := make([]int, len(b)+1)
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			current[j] = min(
				current[j-1]+1,
				previous[j]+1,
				previous[j-1]+cost,
			)
		}
		previous = current
	}
	return previous[len(b)]
}
