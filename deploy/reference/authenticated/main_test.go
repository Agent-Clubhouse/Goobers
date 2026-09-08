package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runnercap"
)

func fixture(t *testing.T) options {
	t.Helper()
	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.InitDemo(root); err != nil {
		t.Fatal(err)
	}
	cfg, err := instance.LoadConfig(filepath.Join(root, "instance.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Engine = &instance.EngineConfig{HostPort: temporalHost, Namespace: "default", TaskQueue: "goobers-engine"}
	image := "registry.example.test/goobers@sha256:" + strings.Repeat("a", 64)
	cfg.Runners = append(cfg.Runners, instance.RunnerEntry{Name: "linux-pod", Host: image, Provides: instance.RunnerProvides{OS: "linux", CPU: "2000m", Memory: "4Gi", Disk: "20Gi", Shell: true}, Restrictions: []instance.RunnerRestriction{instance.RunnerRestriction(runnercap.RestrictionNetworkNone)}})
	if err := instance.WriteConfig(filepath.Join(root, "instance.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	return options{Reference: "..", Instance: root, Out: filepath.Join(t.TempDir(), "prepared"), Image: image, StageNamespace: "gaggle-demo", JournalClass: "block", BlobClass: "shared", TLSSecret: "api-tls", TokenSecret: "pod-key", CAConfigMap: "api-ca", APIServerCIDRs: "10.0.0.1/32,192.0.2.11/32", APIServerPorts: "443,6443"}
}

func generated(t *testing.T, o options) []map[string]any {
	t.Helper()
	if err := prepare(o); err != nil {
		t.Fatal(err)
	}
	docs, err := readDocs(filepath.Join(o.Out, "resources.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return docs
}
func decode(t *testing.T, d map[string]any, out any) {
	t.Helper()
	b, err := yaml.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
}
func volume(t *testing.T, d appsv1.Deployment, name string) corev1.Volume {
	t.Helper()
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("missing volume %s", name)
	return corev1.Volume{}
}

func TestPreparedTopologySeparatesAuthorityAndKeepsConfigImmutable(t *testing.T) {
	o := fixture(t)
	if err := os.WriteFile(filepath.Join(o.Instance, "config", "asset.sh"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	docs := generated(t, o)
	deps := map[string]appsv1.Deployment{}
	var bundle corev1.Secret
	bindingCount := 0
	for _, d := range docs {
		switch d["kind"] {
		case "Deployment":
			var dep appsv1.Deployment
			decode(t, d, &dep)
			deps[dep.Name] = dep
		case "Secret":
			decode(t, d, &bundle)
		case "RoleBinding":
			var rb rbacv1.RoleBinding
			decode(t, d, &rb)
			bindingCount++
			if rb.Namespace != o.StageNamespace || len(rb.Subjects) != 1 || rb.Subjects[0].Name != "goobers-worker" || rb.Subjects[0].Namespace != systemNS {
				t.Fatalf("unexpected authority grant: %+v", rb)
			}
		case "ServiceAccount":
			var sa corev1.ServiceAccount
			decode(t, d, &sa)
			if sa.Name == "goobers-api" || sa.Name == "default" {
				if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
					t.Fatal("API or stage inherited Kubernetes token")
				}
			}
		}
	}
	if len(deps) != 2 || bindingCount != 1 {
		t.Fatalf("want just daemon/worker plus dispatcher grant: %d deployments, %d bindings", len(deps), bindingCount)
	}
	daemon, worker := deps["goobers-api"], deps["goobers-worker"]
	if v := volume(t, daemon, "journal"); v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "goobers-journal" {
		t.Fatal("daemon lost authoritative volume")
	}
	if v := volume(t, worker, "journal"); v.EmptyDir == nil || v.PersistentVolumeClaim != nil {
		t.Fatal("worker shares authoritative instance")
	}
	var paths map[string]string
	if err := json.Unmarshal(bundle.Data["paths.json"], &paths); err != nil {
		t.Fatal(err)
	}
	for _, dep := range deps {
		assetExecutable := false
		for _, item := range volume(t, dep, "config-bundle").Secret.Items {
			if paths[item.Key] == "config/asset.sh" && item.Mode != nil && *item.Mode == 0550 {
				assetExecutable = true
			}
		}
		if !assetExecutable {
			t.Fatal("Secret projection drops executable asset permissions before init copies them")
		}
		if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
			t.Fatal("unsafe concurrent state owners")
		}
		if volume(t, dep, "prepared-config").EmptyDir == nil {
			t.Fatal("prepared configs are not fresh per pod")
		}
		if volume(t, dep, "blobs").PersistentVolumeClaim.ClaimName != "goobers-blobs" {
			t.Fatal("blob backing trees differ")
		}
		if dep.Spec.Template.Annotations["goobers.dev/config-bundle"] != bundle.Name || volume(t, dep, "config-bundle").Secret.SecretName != bundle.Name {
			t.Fatal("rollout template isn't pinned to its bundle")
		}
		if got := volume(t, dep, "pod-auth").Secret; got.SecretName != o.TokenSecret || got.DefaultMode == nil || *got.DefaultMode != 0440 {
			t.Fatal("missing shared key or overly broad permissions")
		}
		mounts := map[string]bool{}
		for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
			mounts[m.MountPath] = m.ReadOnly
		}
		for _, path := range []string{"/var/lib/goobers/instance.yaml", "/var/lib/goobers/config", "/var/lib/goobers/goobers"} {
			if !mounts[path] {
				t.Fatalf("daemon/worker can mutate managed config at %s", path)
			}
		}
	}
	for _, v := range worker.Spec.Template.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == o.TLSSecret {
			t.Fatal("worker obtained daemon TLS private key")
		}
	}
	if bundle.Immutable == nil || !*bundle.Immutable {
		t.Fatal("bundle is mutable")
	}
	root := unpackBundle(t, bundle.Data)
	cfg, err := instance.LoadConfig(filepath.Join(root, "instance.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.Listen != "0.0.0.0:8080" || cfg.API.PodTokenKeyFile != "/run/goobers/pod-auth/pod-token.key" || cfg.API.TLS == nil {
		t.Fatal("authenticated listener was not wired")
	}
	if _, _, err := instance.LoadConfigDir(filepath.Join(root, "config")); err != nil {
		t.Fatalf("prepared config is not loadable: %v", err)
	}
	args := strings.Join(worker.Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--daemon-api "+apiURL) || !strings.Contains(args, "--dispatch-namespace "+o.StageNamespace) {
		t.Fatal("worker cannot surrender")
	}
	found := false
	for _, e := range worker.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "GOOBERS_BLOB_ENDPOINT" && e.Value == apiURL {
			found = true
		}
	}
	if !found {
		t.Fatal("worker kit/output blob endpoint missing")
	}
}

func unpackBundle(t *testing.T, data map[string][]byte) string {
	t.Helper()
	root := t.TempDir()
	var index map[string]string
	if err := json.Unmarshal(data["paths.json"], &index); err != nil {
		t.Fatal(err)
	}
	for key, name := range index {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data[key], 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPreparedTopologyConfigChangeRollsBothPodsAndDropsRemovedFiles(t *testing.T) {
	o := fixture(t)
	asset := filepath.Join(o.Instance, "config", "notes.txt")
	if err := os.WriteFile(asset, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	first := generated(t, o)
	if err := os.Remove(asset); err != nil {
		t.Fatal(err)
	}
	o.Out = filepath.Join(t.TempDir(), "updated")
	second := generated(t, o)
	names := func(docs []map[string]any) map[string]string {
		n := map[string]string{}
		for _, d := range docs {
			switch d["kind"] {
			case "Secret":
				var s corev1.Secret
				decode(t, d, &s)
				n["bundle"] = s.Name
			case "Deployment":
				var dep appsv1.Deployment
				decode(t, d, &dep)
				n[dep.Name] = volume(t, dep, "config-bundle").Secret.SecretName
			}
		}
		return n
	}
	a, b := names(first), names(second)
	if a["bundle"] == b["bundle"] {
		t.Fatal("config changes preserve bundle name")
	}
	for _, dep := range []string{"goobers-api", "goobers-worker"} {
		if a[dep] == b[dep] || b[dep] != b["bundle"] {
			t.Fatal("config update does not roll both deployments")
		}
	}
	for _, d := range second {
		if d["kind"] == "Secret" {
			var s corev1.Secret
			decode(t, d, &s)
			root := unpackBundle(t, s.Data)
			if _, err := os.Stat(filepath.Join(root, "config", "notes.txt")); !os.IsNotExist(err) {
				t.Fatal("removed config file survives next bundle")
			}
		}
	}
}

func TestPreparedTopologyNetworkPoliciesMatchRealStageLabels(t *testing.T) {
	o := fixture(t)
	docs := generated(t, o)
	pod, err := dispatcher.RenderPod(dispatcher.Config{Namespace: o.StageNamespace}, dispatcher.Attempt{RunID: "run-1", Stage: "probe", Number: 1}, dispatcher.RunnerSpec{Name: "linux-pod", OS: "linux", Host: o.Image, HostKind: instance.RunnerHostImage, Restrictions: []string{string(runnercap.RestrictionNetworkNone)}})
	if err != nil {
		t.Fatal(err)
	}
	stageGrant, apiIngress, apiEgress, defaultDenies := false, false, false, 0
	for _, d := range docs {
		if d["kind"] != "NetworkPolicy" {
			continue
		}
		var p networkingv1.NetworkPolicy
		decode(t, d, &p)
		if p.Name == "default-deny-all" {
			if len(p.Spec.PolicyTypes) != 2 || len(p.Spec.Ingress) != 0 || len(p.Spec.Egress) != 0 {
				t.Fatal("default deny lost")
			}
			defaultDenies++
		}
		if p.Name == "allow-worker-apiserver" {
			apiEgress = true
			for _, e := range p.Spec.Egress {
				if len(e.To) != 2 || len(e.Ports) != 2 {
					t.Fatal("API addresses/ports missing")
				}
			}
		}
		if p.Name == "allow-dispatch-clients" {
			for _, r := range p.Spec.Ingress {
				for _, peer := range r.From {
					if peer.NamespaceSelector != nil {
						if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != o.StageNamespace || peer.PodSelector == nil {
							t.Fatal("cross-namespace peer became OR grant")
						}
						for k, v := range peer.PodSelector.MatchLabels {
							if pod.Labels[k] != v {
								t.Fatalf("real stage pod cannot reach daemon: %s", k)
							}
						}
						apiIngress = true
					}
				}
			}
		}
		if p.Namespace == o.StageNamespace && p.Spec.PodSelector.MatchLabels[runnercap.LabelRunnerClass] == pod.Labels[runnercap.LabelRunnerClass] {
			for _, e := range p.Spec.Egress {
				for _, peer := range e.To {
					if peer.NamespaceSelector != nil && peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == systemNS {
						if peer.PodSelector == nil || peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "goobers-api" {
							t.Fatal("blob grant widened")
						}
						stageGrant = true
					}
				}
			}
		}
	}
	if !stageGrant || !apiIngress || !apiEgress || defaultDenies != 2 {
		t.Fatalf("missing enforced path: stage=%v ingress=%v kube=%v denies=%d", stageGrant, apiIngress, apiEgress, defaultDenies)
	}
}

func TestPreparationFailsBeforePublishingInvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*options)
	}{
		{"floating image", func(o *options) { o.Image = "example.test/goobers:latest" }},
		{"broad API CIDR", func(o *options) { o.APIServerCIDRs = "10.0.0.0/8" }},
		{"shared namespace", func(o *options) { o.StageNamespace = systemNS }},
		{"symlink", func(o *options) {
			if err := os.Symlink("/etc/passwd", filepath.Join(o.Instance, "config", "leak")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := fixture(t)
			tc.mutate(&o)
			if err := prepare(o); err == nil {
				t.Fatal("invalid topology published")
			}
			if _, err := os.Stat(o.Out); !os.IsNotExist(err) {
				t.Fatal("invalid topology left output")
			}
		})
	}
}

func TestBundleTracksExecutableAssetsAndRefusesShellMetacharacters(t *testing.T) {
	o := fixture(t)
	asset := filepath.Join(o.Instance, "config", "script.sh")
	if err := os.WriteFile(asset, []byte("#!/bin/sh\nexit 0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	a, _, err := configBundle(o.Instance, []byte("config"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(asset, 0755); err != nil {
		t.Fatal(err)
	}
	b, command, err := configBundle(o.Instance, []byte("config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(a["modes.json"]) == string(b["modes.json"]) {
		t.Fatal("executable change not included in bundle identity")
	}
	var index map[string]string
	var modes map[string]int32
	if err := json.Unmarshal(b["paths.json"], &index); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b["modes.json"], &modes); err != nil {
		t.Fatal(err)
	}
	for key, name := range index {
		if name == "config/script.sh" && modes[key] != 0550 {
			t.Fatal("executable asset lost execution permissions")
		}
	}
	if strings.Contains(command, "tar") || strings.Contains(command, "gzip") {
		t.Fatal("init needs unavailable archive tools")
	}
	if err := os.WriteFile(filepath.Join(o.Instance, "config", "$(id).txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := configBundle(o.Instance, nil); err == nil {
		t.Fatal("untrusted filename entered the shell/Kubernetes expansion program")
	}
}

func TestBundleRefusesOversizedSecret(t *testing.T) {
	o := fixture(t)
	if err := os.WriteFile(filepath.Join(o.Instance, "config", "oversize.txt"), make([]byte, 701<<10), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepare(o); err == nil {
		t.Fatal("oversized Kubernetes Secret published")
	}
	if _, err := os.Stat(o.Out); !os.IsNotExist(err) {
		t.Fatal("oversized bundle left partial output")
	}
}
