package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestWorkflowNoopProcess(t *testing.T) {}

func TestWorkflowFailureProcess(t *testing.T) {
	for _, arg := range os.Args[1:] {
		if arg == "goobers-workflow-failure" {
			os.Exit(1)
		}
	}
}

func portableWorkflowCommands(t *testing.T, body string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []struct {
		name string
		test string
	}{
		{"true", "TestWorkflowNoopProcess"},
		{"false", "TestWorkflowFailureProcess"},
	} {
		args := []string{executable, "-test.run=^" + command.test + "$"}
		if command.name == "false" {
			args = append(args, "--", "goobers-workflow-failure")
		}
		argv, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		body = strings.ReplaceAll(body, `command: ["`+command.name+`"]`, "command: "+string(argv))
	}
	return body
}
