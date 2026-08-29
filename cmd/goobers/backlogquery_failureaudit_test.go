package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

func TestBacklogQueryHasNoShippedFailBranchConsumers(t *testing.T) {
	configRoots := []string{
		filepath.Join("..", "..", "reference-workflows"),
		filepath.Join("..", "..", "config-examples"),
		filepath.Join("..", "..", "examples", "ios-simulator"),
		filepath.Join("..", "..", "internal", "instance", "starter"),
		filepath.Join("..", "..", "internal", "instance", "quickstart-v1"),
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

	const wantProducers = 16
	if producers != wantProducers {
		t.Fatalf("found %d shipped backlog-query stages, want audited inventory of %d", producers, wantProducers)
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

func TestBacklogQueryFatalProviderPathInventory(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve audit test path")
	}
	path := filepath.Join(filepath.Dir(testFile), "backlogquery.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	got := map[string]int{}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		function, ok := call.Fun.(*ast.Ident)
		if !ok || function.Name != "failProviderStage" || len(call.Args) < 2 {
			return true
		}
		what, ok := call.Args[1].(*ast.BasicLit)
		if !ok || what.Kind != token.STRING {
			t.Fatalf("failProviderStage in %s must use a literal operation name", path)
		}
		name, err := strconv.Unquote(what.Value)
		if err != nil {
			t.Fatalf("decode failProviderStage operation %s: %v", what.Value, err)
		}
		got[name]++
		return true
	})

	want := map[string]int{
		"reconcile backlog metadata":                2,
		"list open pull requests":                   1,
		"reconcile closed pull requests":            1,
		"list work items":                           2,
		"list blocked items for dependency recheck": 1,
		"list ready items for re-sweep":             1,
		"read ready-label transitions":              1,
		"compute claimed-item staleness":            1,
		"compute read-only re-sweep staleness":      1,
		"release backlog claims":                    1,
		"verify decomposition publication barrier":  2,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fatal provider path inventory = %v, want %v; add fault-injection coverage for any new path", got, want)
	}
}
