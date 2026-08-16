package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

type deploymentCommandContract struct {
	binary        string
	command       string
	requiredFlags []string
}

func TestDeployReferenceContainerArgsMatchCLIRegistry(t *testing.T) {
	contracts := map[string]deploymentCommandContract{
		"goobers-api":      {binary: "goobers", command: "up"},
		"goobers-operator": {binary: "goobers-operator"},
		"goobers-worker":   {binary: "goobers", command: "worker", requiredFlags: []string{"instance"}},
	}

	paths, err := filepath.Glob("../../deploy/reference/goobers-system/*-deployment.yaml")
	if err != nil {
		t.Fatalf("glob Deployment manifests: %v", err)
	}
	if len(paths) != len(contracts) {
		t.Fatalf("found %d Deployment manifests, want %d", len(paths), len(contracts))
	}

	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var deployment appsv1.Deployment
		if err := yaml.Unmarshal(raw, &deployment); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		contract, ok := contracts[deployment.Name]
		if !ok {
			t.Errorf("%s: no CLI contract for Deployment %q", path, deployment.Name)
			continue
		}
		if len(deployment.Spec.Template.Spec.Containers) != 1 {
			t.Errorf("%s: got %d containers, want 1", path, len(deployment.Spec.Template.Spec.Containers))
			continue
		}
		container := deployment.Spec.Template.Spec.Containers[0]
		binary := "goobers"
		if len(container.Command) > 0 {
			if len(container.Command) != 1 {
				t.Errorf("%s: command = %v, want one binary", path, container.Command)
				continue
			}
			binary = container.Command[0]
		}
		if binary != contract.binary {
			t.Errorf("%s: binary = %q, want %q", path, binary, contract.binary)
			continue
		}
		if contract.command == "" {
			if len(container.Args) != 0 {
				t.Errorf("%s: standalone binary %q has unexpected args %v", path, binary, container.Args)
			}
			continue
		}
		if len(container.Args) == 0 || container.Args[0] != contract.command {
			t.Errorf("%s: args = %v, want command %q", path, container.Args, contract.command)
			continue
		}

		fs := registeredCommandFlagSet(t, contract.command)
		if err := validateManifestArgs(container.Args[1:], fs, contract.requiredFlags); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}

	workerFlags := registeredCommandFlagSet(t, "worker")
	if err := validateManifestArgs([]string{"--task-queue", "default"}, workerFlags, []string{"instance"}); err == nil {
		t.Error("worker args without --instance were accepted")
	}
	if err := validateManifestArgs([]string{"--instance", "/instance", "--not-a-worker-flag"}, workerFlags, []string{"instance"}); err == nil {
		t.Error("unknown worker flag was accepted")
	}
}

func registeredCommandFlagSet(t *testing.T, command string) *flag.FlagSet {
	t.Helper()
	registration, ok := commandHelp(command)
	if !ok {
		t.Fatalf("CLI registry has no command %q", command)
	}

	var observed *flag.FlagSet
	cliFlagSetObserverMu.Lock()
	previous := cliFlagSetObserver
	cliFlagSetObserver = func(id string, fs *flag.FlagSet) {
		if id == command {
			observed = fs
		}
	}
	cliFlagSetObserverMu.Unlock()
	defer func() {
		cliFlagSetObserverMu.Lock()
		cliFlagSetObserver = previous
		cliFlagSetObserverMu.Unlock()
	}()

	registration.run([]string{"-h"}, io.Discard, io.Discard)
	if observed == nil {
		t.Fatalf("command %q did not construct its registered flag set", command)
	}
	return observed
}

func validateManifestArgs(args []string, fs *flag.FlagSet, required []string) error {
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue
		}
		nameValue := strings.TrimLeft(arg, "-")
		name, value, hasValue := strings.Cut(nameValue, "=")
		registered := fs.Lookup(name)
		if registered == nil {
			return fmt.Errorf("unknown flag %q for %s", arg, fs.Name())
		}
		seen[name] = true
		if hasValue {
			if value == "" {
				return fmt.Errorf("flag %q has an empty value", arg)
			}
			continue
		}
		boolFlag, isBool := registered.Value.(interface{ IsBoolFlag() bool })
		if isBool && boolFlag.IsBoolFlag() {
			continue
		}
		i++
		if i == len(args) || strings.HasPrefix(args[i], "-") {
			return fmt.Errorf("flag %q is missing its value", arg)
		}
	}
	for _, name := range required {
		if !seen[name] {
			return fmt.Errorf("required flag --%s is missing", name)
		}
	}
	return nil
}
