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
	"github.com/goobers/goobers/internal/netpolrender"

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

func TestDeployReferenceBlobPlaneServicePortMatchesDefaultEndpoint(t *testing.T) {
	wantPort := int32(netpolrender.DefaultBlobEndpoint().Port)

	serviceRaw, err := os.ReadFile("../../deploy/reference/goobers-system/api-service.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var service corev1.Service
	if err := yaml.Unmarshal(serviceRaw, &service); err != nil {
		t.Fatal(err)
	}
	if got := len(service.Spec.Ports); got != 1 {
		t.Fatalf("Service ports = %d, want 1", got)
	}
	if got := service.Spec.Ports[0].Port; got != wantPort {
		t.Fatalf("Service port = %d, want %d from netpolrender.DefaultBlobEndpoint()", got, wantPort)
	}

	deploymentRaw, err := os.ReadFile("../../deploy/reference/goobers-system/api-deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(deploymentRaw, &deployment); err != nil {
		t.Fatal(err)
	}
	containers := deployment.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(containers))
	}
	if got := containers[0].Ports[0].ContainerPort; got != wantPort {
		t.Fatalf("API container port = %d, want %d from netpolrender.DefaultBlobEndpoint()", got, wantPort)
	}
}

// TestDeployReferenceAPIProbes locks the reference api-deployment.yaml's
// startup/readiness/liveness probe shape (#3806): each MUST target the
// correct path over HTTPS. internal/httpapi/server.go refuses to serve a
// non-loopback bind without TLS (SEC-043/#640), so any daemon a kubelet can
// reach at podIP:port is necessarily HTTPS — an httpGet probe with no
// scheme (kubelet defaults to HTTP) gets a 400 from Go's TLS listener on
// every attempt, CrashLooping the pod. Nothing else in this repo's test
// suite would catch that manifest regressing: deleting the probes entirely
// still passes `make deploy-validate` (kubeconform + the cross-base
// assertion), since neither validates probe content.
func TestDeployReferenceAPIProbes(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/reference/goobers-system/api-deployment.yaml")
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
	container := containers[0]

	checkProbe := func(name string, probe *corev1.Probe, wantPath string) {
		t.Helper()
		if probe == nil {
			t.Fatalf("%s: missing", name)
		}
		if probe.HTTPGet == nil {
			t.Fatalf("%s: not an httpGet probe: %+v", name, probe)
		}
		if probe.HTTPGet.Path != wantPath {
			t.Errorf("%s: path = %q, want %q", name, probe.HTTPGet.Path, wantPath)
		}
		if probe.HTTPGet.Port.StrVal != "http" && probe.HTTPGet.Port.IntVal != 8080 {
			t.Errorf("%s: port = %v, want the api container's named/numeric port", name, probe.HTTPGet.Port)
		}
		// #3806 mustFix: kubelet defaults httpGet.Scheme to HTTP; the daemon
		// this probe targets serves HTTPS on the only posture where a
		// kubelet probe can reach it at all (SEC-043/#640, non-loopback
		// binds require TLS). A missing/wrong scheme here CrashLoops the
		// pod against every real deployed instance.
		if probe.HTTPGet.Scheme != corev1.URISchemeHTTPS {
			t.Errorf("%s: scheme = %q, want %q — kubelet's HTTP default cannot reach this daemon's TLS listener", name, probe.HTTPGet.Scheme, corev1.URISchemeHTTPS)
		}
		if probe.TimeoutSeconds <= 0 {
			t.Errorf("%s: timeoutSeconds = %d, want an explicit positive value (kubelet's 1s default is tight for a real network+TLS round trip)", name, probe.TimeoutSeconds)
		}
	}

	checkProbe("startupProbe", container.StartupProbe, "/readyz")
	checkProbe("readinessProbe", container.ReadinessProbe, "/readyz")
	checkProbe("livenessProbe", container.LivenessProbe, "/healthz")
}

func TestDeployReferenceWorkerInitContainerIsRestrictedCompatible(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/reference/goobers-system/worker-deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(raw, &deployment); err != nil {
		t.Fatal(err)
	}

	initContainers := deployment.Spec.Template.Spec.InitContainers
	if len(initContainers) != 1 {
		t.Fatalf("got %d init containers, want 1", len(initContainers))
	}
	seed := initContainers[0]
	if seed.Name != "seed-instance-root" {
		t.Fatalf("init container name = %q, want %q", seed.Name, "seed-instance-root")
	}
	if seed.SecurityContext == nil {
		t.Fatal("init container has no security context")
	}
	if seed.SecurityContext.AllowPrivilegeEscalation == nil || *seed.SecurityContext.AllowPrivilegeEscalation {
		t.Error("init container allowPrivilegeEscalation = true, want false")
	}
	if seed.SecurityContext.ReadOnlyRootFilesystem == nil || !*seed.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("init container readOnlyRootFilesystem = false, want true")
	}
	if seed.SecurityContext.Capabilities == nil || !slices.Equal(seed.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Errorf("init container capabilities.drop = %v, want [ALL]", seed.SecurityContext.Capabilities)
	}
	if deployment.Spec.Template.Spec.SecurityContext == nil ||
		deployment.Spec.Template.Spec.SecurityContext.SeccompProfile == nil ||
		deployment.Spec.Template.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("init container does not inherit a RuntimeDefault seccomp profile")
	}
}

// TestDeployReferenceDisabledDeploymentsShipZeroReplicas locks the reference
// base's disabled components at replicas: 0 (#3277). The daemon API waits on
// its in-cluster listener (#652); the operator is quarantined Tier-3
// (internal/operator package doc, docs/ARCHITECTURE.md §11) and its reconciler
// schedules a runtime image nothing in this repository publishes, so applying
// the base as shipped would reconcile CRDs into ImagePullBackOff workloads.
// `make deploy-validate` checks schema, not semantics, so nothing else here
// would catch either regressing back to a running replica.
func TestDeployReferenceDisabledDeploymentsShipZeroReplicas(t *testing.T) {
	for _, name := range []string{"api", "operator"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "reference", "goobers-system", name+"-deployment.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var deployment appsv1.Deployment
		if err := yaml.Unmarshal(raw, &deployment); err != nil {
			t.Fatal(err)
		}
		if deployment.Spec.Replicas == nil {
			t.Errorf("%s-deployment.yaml: replicas is unset, want an explicit 0", name)
			continue
		}
		if *deployment.Spec.Replicas != 0 {
			t.Errorf("%s-deployment.yaml: replicas = %d, want 0 — this component is not ready to run in the reference base", name, *deployment.Spec.Replicas)
		}
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
