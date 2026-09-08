// Command authenticated prepares an opt-in, single-gaggle reference topology.
// It only writes local manifests; it never contacts a cluster.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/netpolrender"
)

const systemNS = "goobers-system"
const apiURL = "https://goobers-api.goobers-system.svc:8080"
const temporalHost = "temporal-frontend.goobers-temporal:7233"

type options struct {
	Reference, Instance, Out, Image, StageNamespace                                 string
	JournalClass, BlobClass, TLSSecret, TokenSecret, CAConfigMap, CredentialsSecret string
	APIServerCIDRs                                                                  string
	APIServerPorts                                                                  string
}

func main() {
	var o options
	flag.StringVar(&o.Reference, "reference", "deploy/reference", "reference source directory")
	flag.StringVar(&o.Instance, "instance", "", "prepared local instance; one gaggle, digest-pinned image runners")
	flag.StringVar(&o.Out, "out", "", "new output directory (must not exist)")
	flag.StringVar(&o.Image, "image", "", "control-plane and init image, pinned with @sha256:")
	flag.StringVar(&o.StageNamespace, "stage-namespace", "", "dedicated namespace for the single gaggle")
	flag.StringVar(&o.JournalClass, "journal-storage-class", "", "RWO block storage class")
	flag.StringVar(&o.BlobClass, "blob-storage-class", "", "RWX artifact storage class")
	flag.StringVar(&o.TLSSecret, "tls-secret", "", "existing TLS Secret in goobers-system: tls.crt, tls.key")
	flag.StringVar(&o.TokenSecret, "pod-token-secret", "", "existing shared-key Secret in goobers-system: pod-token.key")
	flag.StringVar(&o.CAConfigMap, "ca-configmap", "", "existing ConfigMap in goobers-system: ca.crt (public roots plus API CA)")
	flag.StringVar(&o.CredentialsSecret, "credentials-secret", "", "optional existing Secret mounted at /run/goobers/credentials")
	flag.StringVar(&o.APIServerCIDRs, "apiserver-cidrs", "", "comma-separated exact API endpoint /32 or /128 CIDRs, including required Service IP")
	flag.StringVar(&o.APIServerPorts, "apiserver-ports", "443", "comma-separated API TCP ports required by the CNI, e.g. 443,6443")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected arguments")
		os.Exit(2)
	}
	if err := prepare(o); err != nil {
		fmt.Fprintln(os.Stderr, "prepare authenticated topology:", err)
		os.Exit(1)
	}
	fmt.Println("Prepared local manifests in", o.Out, "; no cluster changes made")
}

var imageDigest = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[a-f0-9]{64}$`)

type apiAccess struct {
	peers []networkingv1.NetworkPolicyPeer
	ports []networkingv1.NetworkPolicyPort
}

type preparedBundle struct {
	secret      *corev1.Secret
	initCommand string
}

func prepare(o options) error {
	if err := validateTopologyOptions(o); err != nil {
		return err
	}
	access, err := parseAPIAccess(o)
	if err != nil {
		return err
	}
	cfg, set, err := loadTopologyConfig(o)
	if err != nil {
		return err
	}
	input, err := topologyNetworkInput(cfg)
	if err != nil {
		return err
	}
	rendered, err := netpolrender.Render(input)
	if err != nil {
		return err
	}
	bundle, err := prepareBundle(o.Instance, cfg)
	if err != nil {
		return err
	}
	objects, err := controlPlaneResources(o, cfg, bundle)
	if err != nil {
		return err
	}
	network, err := controlPlaneNetwork(o, input, access)
	if err != nil {
		return err
	}
	stages, err := stageResources(o, set.Gaggles[0].Name, rendered.Files)
	if err != nil {
		return err
	}
	objects = append(objects, network...)
	objects = append(objects, stages...)
	return publishTopology(o.Out, objects)
}

func validateTopologyOptions(o options) error {

	if o.Instance == "" || o.Out == "" {
		return fmt.Errorf("--instance and --out are required")
	}
	if !imageDigest.MatchString(o.Image) {
		return fmt.Errorf("--image must be an immutable sha256 image reference")
	}
	for name, value := range map[string]string{"stage-namespace": o.StageNamespace, "journal-storage-class": o.JournalClass, "blob-storage-class": o.BlobClass, "tls-secret": o.TLSSecret, "pod-token-secret": o.TokenSecret, "ca-configmap": o.CAConfigMap} {
		if len(kvalidation.IsDNS1123Subdomain(value)) != 0 || strings.Contains(strings.ToLower(value), "change-me") {
			return fmt.Errorf("--%s must name an explicit Kubernetes resource", name)
		}
	}
	if len(kvalidation.IsDNS1123Label(o.StageNamespace)) != 0 || o.StageNamespace == systemNS || o.StageNamespace == "goobers-temporal" || o.StageNamespace == "kube-system" {
		return fmt.Errorf("stage namespace must be a separate DNS-label namespace")
	}
	if o.CredentialsSecret != "" && len(kvalidation.IsDNS1123Subdomain(o.CredentialsSecret)) != 0 {
		return fmt.Errorf("invalid credentials-secret name")
	}
	if o.TLSSecret == o.TokenSecret || o.TLSSecret == o.CredentialsSecret {
		return fmt.Errorf("daemon TLS private key must use a different Secret from worker-readable credentials")
	}
	return nil
}

func parseAPIAccess(o options) (apiAccess, error) {

	peers := []networkingv1.NetworkPolicyPeer{}
	for _, raw := range strings.Split(o.APIServerCIDRs, ",") {
		p, err := netip.ParsePrefix(raw)
		if err != nil || p.Bits() != p.Addr().BitLen() || p.Addr().IsUnspecified() || p.Addr().IsMulticast() || p.Addr().IsLoopback() {
			return apiAccess{}, fmt.Errorf("apiserver-cidrs must contain exact /32 or /128 endpoint addresses")
		}
		peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: p.String()}})
	}
	ports := []networkingv1.NetworkPolicyPort{}
	for _, raw := range strings.Split(o.APIServerPorts, ",") {
		var n int
		if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || fmt.Sprint(n) != raw || n < 1 || n > 65535 {
			return apiAccess{}, fmt.Errorf("apiserver-ports must contain TCP ports in 1..65535")
		}
		ports = append(ports, tcp(n))
	}
	return apiAccess{peers: peers, ports: ports}, nil
}

func loadTopologyConfig(o options) (*instance.Config, *instance.ConfigSet, error) {

	cfg, err := instance.LoadConfig(filepath.Join(o.Instance, "instance.yaml"))
	if err != nil {
		return nil, nil, err
	}
	if cfg.WorkflowSource != nil {
		return nil, nil, fmt.Errorf("bundle-managed topology requires local config, without workflowSource sync")
	}
	if cfg.Engine == nil || cfg.Engine.HostPort != temporalHost {
		return nil, nil, fmt.Errorf("engine.hostPort must be %s; configure Temporal separately before use", temporalHost)
	}
	engine, _, err := cfg.ResolveEngineConfig(func(string) (string, bool) { return "", false })
	if err != nil {
		return nil, nil, err
	}
	cfg.Engine.HostPort, cfg.Engine.Namespace, cfg.Engine.TaskQueue = engine.HostPort, engine.Namespace, engine.TaskQueue
	set, report, err := instance.LoadConfigDir(filepath.Join(o.Instance, "config"))
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w (%v)", err, report)
	}
	if len(set.Gaggles) != 1 {
		return nil, nil, fmt.Errorf("this topology requires exactly one manifest-listed gaggle")
	}
	return cfg, set, nil
}

func topologyNetworkInput(cfg *instance.Config) (netpolrender.Input, error) {

	input := netpolrender.Input{}
	for _, entry := range cfg.ResolvedRunners() {
		kind, err := instance.ClassifyRunnerHost(entry.Host)
		if err != nil {
			return netpolrender.Input{}, err
		}
		if kind == instance.RunnerHostSelf {
			return netpolrender.Input{}, fmt.Errorf("runner %s selects self; this topology requires every declared runner to execute in a stage image", entry.Name)
		}
		if kind != instance.RunnerHostImage || !imageDigest.MatchString(entry.Host) {
			return netpolrender.Input{}, fmt.Errorf("runner %s must use a digest-pinned image; deployment templates are outside this topology", entry.Name)
		}
		rs := []string{}
		for _, r := range entry.Restrictions {
			rs = append(rs, string(r))
		}
		input.Runners = append(input.Runners, netpolrender.Runner{Name: entry.Name, Restrictions: rs})
	}
	if len(input.Runners) == 0 {
		return netpolrender.Input{}, fmt.Errorf("at least one non-self image runner is required")
	}
	if cfg.Egress != nil {
		for _, g := range cfg.Egress.Allowlist {
			input.Allowlist = append(input.Allowlist, netpolrender.AllowlistGroup{Name: g.Name, Kind: g.Kind, Source: g.Source, SourceSHA256: g.SourceSHA256, CIDRs: g.CIDRs, Ports: g.Ports})
		}
	}
	return input, nil
}

func prepareBundle(root string, cfg *instance.Config) (preparedBundle, error) {

	cfg.API.Listen = "0.0.0.0:8080"
	cfg.API.TLS = &instance.APITLSConfig{CertFile: "/run/goobers/tls/tls.crt", KeyFile: "/run/goobers/tls/tls.key"}
	cfg.API.PodTokenKeyFile = "/run/goobers/pod-auth/pod-token.key"
	configBytes, err := yaml.Marshal(cfg)
	if err != nil {
		return preparedBundle{}, err
	}
	data, initCommand, err := configBundle(root, configBytes)
	if err != nil {
		return preparedBundle{}, err
	}
	identity, err := json.Marshal(data)
	if err != nil {
		return preparedBundle{}, err
	}
	sum := sha256.Sum256(identity)
	bundleName := "goobers-config-" + hex.EncodeToString(sum[:])[:24]
	immutable := true
	secret := &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: bundleName, Namespace: systemNS}, Immutable: &immutable, Type: corev1.SecretTypeOpaque, Data: data}

	return preparedBundle{secret: secret, initCommand: initCommand}, nil
}

func controlPlaneResources(o options, cfg *instance.Config, bundle preparedBundle) ([]any, error) {
	objects := []any{bundle.secret}

	// Keep the base's selectors/probes/security posture, but omit its disabled
	// operator, Windows deployment, example ingress, and unrelated CRD RBAC.
	for _, name := range []string{"namespace.yaml", "api-rbac.yaml", "api-service.yaml"} {
		docs, err := readDocs(filepath.Join(o.Reference, "goobers-system", name))
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			objects = append(objects, d)
		}
	}
	objects = append(objects, &corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metav1.ObjectMeta{Name: "goobers-worker", Namespace: systemNS}})
	for _, name := range []string{"journal-pvc.yaml", "blobs-pvc.yaml"} {
		var pvc corev1.PersistentVolumeClaim
		if err := readObject(filepath.Join(o.Reference, "goobers-system", name), &pvc); err != nil {
			return nil, err
		}
		class := o.JournalClass
		if name == "blobs-pvc.yaml" {
			class = o.BlobClass
		}
		pvc.Spec.StorageClassName = &class
		objects = append(objects, &pvc)
	}
	for _, daemon := range []bool{true, false} {
		dep, err := deployment(o, bundle.secret.Name, bundle.initCommand, bundle.secret.Data, daemon, cfg)
		if err != nil {
			return nil, err
		}
		objects = append(objects, dep)
	}
	return objects, nil
}

func controlPlaneNetwork(o options, input netpolrender.Input, access apiAccess) ([]any, error) {
	var objects []any

	floor, err := readDocs(filepath.Join(o.Reference, "goobers-system", "networkpolicies.yaml"))
	if err != nil {
		return nil, err
	}
	for _, d := range floor {
		name := d["metadata"].(map[string]any)["name"]
		if name == "default-deny-all" || name == "allow-dns" || name == "allow-worker-temporal" || name == "allow-daemon-temporal" {
			objects = append(objects, d)
		}
	}
	objects = append(objects, policy("allow-worker-apiserver", systemNS, labels("goobers-worker"), nil, []networkingv1.NetworkPolicyEgressRule{{To: access.peers, Ports: access.ports}}))
	apiPeer := networkingv1.NetworkPolicyPeer{PodSelector: selector(labels("goobers-api"))}
	objects = append(objects, policy("allow-worker-api", systemNS, labels("goobers-worker"), nil, []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{apiPeer}, Ports: []networkingv1.NetworkPolicyPort{tcp(8080)}}}))
	// Both selector halves occupy ONE peer; a sibling namespace cannot use it.
	stagePeer := networkingv1.NetworkPolicyPeer{NamespaceSelector: selector(map[string]string{"kubernetes.io/metadata.name": o.StageNamespace}), PodSelector: selector(map[string]string{"goobers.dev/role": "stage"})}
	objects = append(objects, policy("allow-dispatch-clients", systemNS, labels("goobers-api"), []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: selector(labels("goobers-worker"))}, stagePeer}, Ports: []networkingv1.NetworkPolicyPort{tcp(8080)}}}, nil))
	// The authority and worker contact only the explicitly configured provider,
	// model and sandbox ranges. No implicit public Internet/proxy grant.
	var direct []networkingv1.NetworkPolicyEgressRule
	for _, g := range input.Allowlist {
		var dst []networkingv1.NetworkPolicyPeer
		for _, cidr := range g.CIDRs {
			dst = append(dst, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
		}
		var ps []networkingv1.NetworkPolicyPort
		for _, p := range g.Ports {
			ps = append(ps, tcp(p))
		}
		if len(ps) == 0 {
			ps = []networkingv1.NetworkPolicyPort{tcp(443)}
		}
		if len(dst) > 0 {
			direct = append(direct, networkingv1.NetworkPolicyEgressRule{To: dst, Ports: ps})
		}
	}
	if len(direct) > 0 {
		for _, name := range []string{"goobers-api", "goobers-worker"} {
			objects = append(objects, policy(name+"-configured-egress", systemNS, labels(name), nil, direct))
		}
	}
	return objects, nil
}

func stageResources(o options, gaggle string, files []netpolrender.File) ([]any, error) {
	var objects []any

	objects = append(objects, &corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: o.StageNamespace, Labels: map[string]string{"goobers.dev/gaggle": gaggle, "pod-security.kubernetes.io/enforce": "restricted"}}})
	no := false
	objects = append(objects, &corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: o.StageNamespace}, AutomountServiceAccountToken: &no})
	for _, name := range []string{"networkpolicies.yaml", "dispatcher-rbac.yaml"} {
		docs, err := readDocs(filepath.Join(o.Reference, "gaggle-namespace", "base", name))
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			d["metadata"].(map[string]any)["namespace"] = o.StageNamespace
			objects = append(objects, d)
		}
	}
	for _, file := range files {
		var d map[string]any
		if err := yaml.Unmarshal(file.Content, &d); err != nil {
			return nil, err
		}
		if d["kind"] == "NetworkPolicy" {
			d["metadata"].(map[string]any)["namespace"] = o.StageNamespace
			objects = append(objects, d)
		}
	}
	return objects, nil
}

func publishTopology(out string, objects []any) (err error) {

	var output bytes.Buffer
	for _, object := range objects {
		raw, err := yaml.Marshal(object)
		if err != nil {
			return err
		}
		output.WriteString("---\n")
		output.Write(raw)
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		return fmt.Errorf("output must be a new directory")
	}
	parent := filepath.Dir(out)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(parent, ".authenticated-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(temp)) }()
	if err := os.WriteFile(filepath.Join(temp, "resources.yaml"), output.Bytes(), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(temp, "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - resources.yaml\n"), 0600); err != nil {
		return err
	}
	return os.Rename(temp, out)
}

// The prepared release base has sh/cp/mkdir but deliberately no tar/gzip.
// Flatten the Secret keys and generate a fixed copy program; never execute
// configuration contents or interpolate adopted filenames into shell syntax.
func configBundle(root string, config []byte) (map[string][]byte, string, error) {
	data := map[string][]byte{}
	index := map[string]string{}
	modes := map[string]int32{}
	var command strings.Builder
	command.WriteString("set -eu\nmkdir -p /prepared/config /prepared/goobers\n")
	total := 0
	write := func(name string, b []byte, executable bool) error {
		if !regexp.MustCompile(`^[A-Za-z0-9._/-]+$`).MatchString(name) {
			return fmt.Errorf("bundle filenames must contain only ASCII letters, digits, dot, underscore, slash, or hyphen")
		}
		total += len(b)
		if total > 700<<10 || len(data) >= 2048 {
			return fmt.Errorf("config bundle exceeds 700 KiB or 2048 files; use another delivery system")
		}
		key := fmt.Sprintf("f%06d", len(data))
		data[key], index[key] = b, name
		modes[key] = 0440
		if executable {
			modes[key] = 0550
		}
		fmt.Fprintf(&command, "mkdir -p '/prepared/%s'\ncp '/bundle/%s' '/prepared/%s'\n", filepath.ToSlash(filepath.Dir(name)), key, name)
		return nil
	}
	if err := write("instance.yaml", config, false); err != nil {
		return nil, "", err
	}
	for _, tree := range []string{"config", "goobers"} {
		path := filepath.Join(root, tree)
		if _, err := os.Stat(path); os.IsNotExist(err) && tree == "goobers" {
			continue
		}
		err := filepath.WalkDir(path, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return fmt.Errorf("bundle only accepts regular files, not symlinks or devices")
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			return write(filepath.ToSlash(rel), b, info.Mode()&0111 != 0)
		})
		if err != nil {
			return nil, "", err
		}
	}
	b, err := json.Marshal(index)
	if err != nil {
		return nil, "", err
	}
	if total+len(b) > 700<<10 {
		return nil, "", fmt.Errorf("config bundle and path index exceed 700 KiB")
	}
	data["paths.json"] = b
	modeData, err := json.Marshal(modes)
	if err != nil {
		return nil, "", err
	}
	if total+len(b)+len(modeData) > 700<<10 {
		return nil, "", fmt.Errorf("config bundle and metadata exceed 700 KiB")
	}
	data["modes.json"] = modeData
	return data, command.String(), nil
}

func readObject(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, v)
}
func readDocs(path string) ([]map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var docs []map[string]any
	for _, raw := range bytes.Split(b, []byte("\n---")) {
		var d map[string]any
		if err := yaml.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		if d["kind"] != nil {
			docs = append(docs, d)
		}
	}
	return docs, nil
}
func labels(name string) map[string]string { return map[string]string{"app.kubernetes.io/name": name} }
func selector(labels map[string]string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: labels}
}
func tcp(n int) networkingv1.NetworkPolicyPort {
	protocol := corev1.ProtocolTCP
	p := intstr.FromInt(n)
	return networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &p}
}
func policy(name, namespace string, match map[string]string, ingress []networkingv1.NetworkPolicyIngressRule, egress []networkingv1.NetworkPolicyEgressRule) *networkingv1.NetworkPolicy {
	types := []networkingv1.PolicyType{}
	if ingress != nil {
		types = append(types, networkingv1.PolicyTypeIngress)
	}
	if egress != nil {
		types = append(types, networkingv1.PolicyTypeEgress)
	}
	return &networkingv1.NetworkPolicy{TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: networkingv1.NetworkPolicySpec{PodSelector: *selector(match), PolicyTypes: types, Ingress: ingress, Egress: egress}}
}

func deployment(o options, bundle, initCommand string, data map[string][]byte, daemon bool, cfg *instance.Config) (*appsv1.Deployment, error) {
	name := "worker-deployment.yaml"
	if daemon {
		name = "api-deployment.yaml"
	}
	var d appsv1.Deployment
	if err := readObject(filepath.Join(o.Reference, "goobers-system", name), &d); err != nil {
		return nil, err
	}
	one := int32(1)
	d.Spec.Replicas = &one
	d.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	p := &d.Spec.Template.Spec
	seconds := int64(90)
	p.TerminationGracePeriodSeconds = &seconds
	d.Spec.Template.Annotations = map[string]string{"goobers.dev/config-bundle": bundle}
	mode := int32(0440)
	var modes map[string]int32
	if err := json.Unmarshal(data["modes.json"], &modes); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(modes))
	for key := range modes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	items := make([]corev1.KeyToPath, 0, len(keys))
	for _, key := range keys {
		mode := modes[key]
		items = append(items, corev1.KeyToPath{Key: key, Path: key, Mode: &mode})
	}
	p.Volumes = append(p.Volumes, corev1.Volume{Name: "prepared-config", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		corev1.Volume{Name: "config-bundle", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: bundle, DefaultMode: &mode, Items: items}}},
		corev1.Volume{Name: "pod-auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: o.TokenSecret, DefaultMode: &mode}}},
		corev1.Volume{Name: "api-ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: o.CAConfigMap}}}})
	no := false
	yes := true
	p.InitContainers = []corev1.Container{{Name: "prepare-config", Image: o.Image, Command: []string{"sh", "-c"}, Args: []string{initCommand}, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}, VolumeMounts: []corev1.VolumeMount{{Name: "prepared-config", MountPath: "/prepared"}, {Name: "config-bundle", MountPath: "/bundle", ReadOnly: true}}}}
	c := &p.Containers[0]
	c.Image = o.Image
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "prepared-config", MountPath: "/var/lib/goobers/config", SubPath: "config", ReadOnly: true}, corev1.VolumeMount{Name: "prepared-config", MountPath: "/var/lib/goobers/goobers", SubPath: "goobers", ReadOnly: true}, corev1.VolumeMount{Name: "prepared-config", MountPath: "/var/lib/goobers/instance.yaml", SubPath: "instance.yaml", ReadOnly: true}, corev1.VolumeMount{Name: "pod-auth", MountPath: "/run/goobers/pod-auth", ReadOnly: true}, corev1.VolumeMount{Name: "api-ca", MountPath: "/run/goobers/ca", ReadOnly: true})
	c.Env = append(c.Env, corev1.EnvVar{Name: "SSL_CERT_FILE", Value: "/run/goobers/ca/ca.crt"})
	if o.CredentialsSecret != "" {
		p.Volumes = append(p.Volumes, corev1.Volume{Name: "credentials", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: o.CredentialsSecret, DefaultMode: &mode}}})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "credentials", MountPath: "/run/goobers/credentials", ReadOnly: true})
	}
	if daemon {
		p.AutomountServiceAccountToken = &no
		p.Volumes = append(p.Volumes, corev1.Volume{Name: "api-tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: o.TLSSecret, DefaultMode: &mode}}}, corev1.Volume{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "api-tls", MountPath: "/run/goobers/tls", ReadOnly: true}, corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"})
	} else {
		for i := range p.Volumes {
			if p.Volumes[i].Name == "journal" {
				p.Volumes[i].VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
			}
		}
		c.Args = []string{"worker", "--instance", "/var/lib/goobers", "--work-root", "/var/lib/goobers-worker", "--blob-store", "/var/lib/goobers-blobs", "--dispatch-namespace", o.StageNamespace, "--daemon-api", apiURL, "--task-queue", cfg.Engine.TaskQueue, "--temporal-hostport", cfg.Engine.HostPort, "--temporal-namespace", cfg.Engine.Namespace, "--config-reload-interval", "10s"}
		c.Env = append(c.Env, corev1.EnvVar{Name: "GOOBERS_BLOB_ENDPOINT", Value: apiURL})
	}
	return &d, nil
}
