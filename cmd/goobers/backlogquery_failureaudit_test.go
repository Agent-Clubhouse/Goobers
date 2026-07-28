package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

func TestBacklogQueryHasNoShippedFailBranchConsumers(t *testing.T) {
	configRoots := []string{
		filepath.Join("..", "..", "selfhost"),
		filepath.Join("..", "..", "config-examples"),
		filepath.Join("..", "..", "examples", "ios-simulator"),
	}

	producers := 0
	var consumers []string
	for _, root := range configRoots {
		set, report, err := instance.LoadConfigDir(root)
		if err != nil {
			t.Fatalf("load shipped config %s: %v\n%v", root, err, report)
		}
		for _, definition := range set.Workflows {
			tasks := make(map[string]apiv1.Task, len(definition.Spec.Tasks))
			for _, task := range definition.Spec.Tasks {
				tasks[task.Name] = task
			}
			gates := make(map[string]apiv1.Gate, len(definition.Spec.Gates))
			for _, gate := range definition.Spec.Gates {
				gates[gate.Name] = gate
			}
			for _, producer := range definition.Spec.Tasks {
				if !invokesBacklogQuery(producer) {
					continue
				}
				producers++
				nextGate, ok := gates[producer.Next]
				if !ok {
					continue
				}
				target, ok := nextGate.Branches["fail"]
				if !ok {
					continue
				}
				handoffs := []string{}
				if consumer, isTask := tasks[target]; isTask {
					handoffs = make([]string, 0, len(consumer.InputsFrom))
					for input, output := range consumer.InputsFrom {
						handoffs = append(handoffs, input+"="+output)
					}
				}
				sort.Strings(handoffs)
				consumers = append(consumers, fmt.Sprintf(
					"%s/%s: %s -> %s[fail] -> %s inputsFrom={%s}",
					definition.Spec.Gaggle,
					definition.Name,
					producer.Name,
					nextGate.Name,
					target,
					strings.Join(handoffs, ","),
				))
			}
		}
	}

	if producers == 0 {
		t.Fatal("found no shipped backlog-query stages")
	}
	if len(consumers) != 0 {
		t.Fatalf("backlog-query fail-branch consumer inventory changed; audit required:\n%s", strings.Join(consumers, "\n"))
	}
}

func invokesBacklogQuery(task apiv1.Task) bool {
	if task.Run == nil {
		return false
	}
	for index, argument := range task.Run.Command {
		if argument == "backlog-query" && index > 0 && filepath.Base(task.Run.Command[index-1]) == "goobers" {
			return true
		}
	}
	return false
}
