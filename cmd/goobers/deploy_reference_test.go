package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/app"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

type deploymentCommandContract struct {
	binary       string
	command      string
	validateArgs func([]string) error
}

func TestDeployReferenceContainerArgsMatchCLIRegistry(t *testing.T) {
	contracts := map[string]deploymentCommandContract{
		"goobers-api": {
			binary:  "goobers",
			command: "up",
			validateArgs: func(args []string) error {
				return validateManifestArgs(args, registeredCommandFlagSet(t, "up"), nil, 1)
			},
		},
		"goobers-operator": {
			binary: "goobers-operator",
			validateArgs: func(args []string) error {
				_, err := app.ParseArgs("operator", args, io.Discard, true)
				return err
			},
		},
		"goobers-worker": {
			binary:  "goobers",
			command: "worker",
			validateArgs: func(args []string) error {
				return validateManifestArgs(args, registeredCommandFlagSet(t, "worker"), []string{"instance"}, 0)
			},
		},
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
		args := container.Args
		if contract.command != "" {
			if len(args) == 0 || args[0] != contract.command {
				t.Errorf("%s: args = %v, want command %q", path, args, contract.command)
				continue
			}
			args = args[1:]
		}
		if err := contract.validateArgs(args); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}

	workerFlags := registeredCommandFlagSet(t, "worker")
	if err := validateManifestArgs([]string{"--task-queue", "default"}, workerFlags, []string{"instance"}, 0); err == nil {
		t.Error("worker args without --instance were accepted")
	}
	workerFlags = registeredCommandFlagSet(t, "worker")
	if err := validateManifestArgs([]string{"--instance", "/instance", "--not-a-worker-flag"}, workerFlags, []string{"instance"}, 0); err == nil {
		t.Error("unknown worker flag was accepted")
	}
	workerFlags = registeredCommandFlagSet(t, "worker")
	if err := validateManifestArgs([]string{"--instance", "/instance", "--drain-timeout", "eventually"}, workerFlags, []string{"instance"}, 0); err == nil {
		t.Error("invalid worker flag value was accepted")
	}
	if _, err := app.ParseArgs("operator", []string{"--version=eventually"}, io.Discard, true); err == nil {
		t.Error("invalid operator flag value was accepted")
	}
}

func TestDeployReferenceWorkerProvidesWritableHarnessHome(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/reference/goobers-system/worker-deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(raw, &deployment); err != nil {
		t.Fatal(err)
	}

	containers := deployment.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(containers))
	}
	mounts := containers[0].VolumeMounts
	if !slices.ContainsFunc(mounts, func(mount corev1.VolumeMount) bool {
		return mount.Name == "harness-home" && mount.MountPath == "/home/nonroot"
	}) {
		t.Errorf("volume mounts %v do not provide writable harness home /home/nonroot", mounts)
	}
	if !slices.ContainsFunc(deployment.Spec.Template.Spec.Volumes, func(volume corev1.Volume) bool {
		return volume.Name == "harness-home" && volume.EmptyDir != nil
	}) {
		t.Error("harness-home is not backed by an emptyDir")
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

func validateManifestArgs(args []string, fs *flag.FlagSet, required []string, maxPositionals int) error {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse %s args: %w", fs.Name(), err)
	}
	if fs.NArg() > maxPositionals {
		return fmt.Errorf("%s has unexpected positional args %v", fs.Name(), fs.Args()[maxPositionals:])
	}

	seen := make(map[string]*flag.Flag)
	fs.Visit(func(f *flag.Flag) {
		seen[f.Name] = f
	})
	for _, name := range required {
		f, ok := seen[name]
		if !ok {
			return fmt.Errorf("required flag --%s is missing", name)
		}
		if f.Value.String() == "" {
			return fmt.Errorf("required flag --%s is empty", name)
		}
	}
	return nil
}
