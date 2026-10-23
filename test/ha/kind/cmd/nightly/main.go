package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/temporaltest"
)

type temporalStep struct {
	server string
	chart  string
	image  string
	values []string
}

var temporalLadder = []temporalStep{
	{server: "1.27.2", chart: "temporal.tgz", image: "1.27.2@sha256:38370631dfad000d8024b83541a1d74771418d90c1e8a6a2d6e6995865b77043", values: []string{"test/ha/kind/temporal-values-legacy.yaml"}},
	{server: "1.27.4", chart: "temporal.tgz", image: "1.27.4@sha256:4cf4366ff5f127558161e9d071892519a87681a8df494f03fb228f585a75a782", values: []string{"test/ha/kind/temporal-values-legacy.yaml"}},
	{server: "1.28.4", chart: "temporal-0.67.0.tgz", image: "1.28.4@sha256:465833b37daee69d90cbff8f855e8f3c96e342087405e61131d4930213feceb4", values: []string{"test/ha/kind/temporal-values-legacy.yaml"}},
	{server: "1.29.7", chart: "temporal-0.73.2.tgz", image: "1.29.7@sha256:688da708d0daca6ff8083334a4a7fbbdbc7e70d1cca8e2eda8c05b21fdbd601e", values: []string{"test/ha/kind/temporal-values-legacy.yaml"}},
	{server: "1.30.8", chart: "temporal-0.74.0.tgz", image: "1.30.8@sha256:54c59485b185ad787061f9a2ed72bd31c8efeae2d4144c61f22873df52c3c781", values: []string{"test/ha/kind/temporal-values-legacy.yaml"}},
	{server: "1.31.0", chart: "temporal-1.3.0.tgz", image: "1.31.0@sha256:b021b3b58c3f169634cdbb0451fcc0e69e8190b40454323362c7c52bbd4ff7b9", values: []string{"deploy/reference/temporal/values.yaml", "test/ha/kind/temporal-values.yaml"}},
}

func main() {
	if len(os.Args) != 2 {
		exitf("usage: nightly run|collect")
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = run()
	case "collect":
		err = collect()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		exitf("%v", err)
	}
}

func run() error {
	assets, err := requiredEnv("HA_ASSET_DIR")
	if err != nil {
		return err
	}
	bin, err := requiredEnv("HA_BIN_DIR")
	if err != nil {
		return err
	}
	evidence, err := requiredEnv("HA_EVIDENCE_DIR")
	if err != nil {
		return err
	}
	toxiproxyURL, err := requiredEnv("TOXIPROXY_API_URL")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(evidence, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Minute)
	defer cancel()
	path := bin + string(os.PathListSeparator) + os.Getenv("PATH")
	env := append(os.Environ(), "PATH="+path, "GOOBERS_TEMPORAL_CLI="+filepath.Join(bin, "temporal"))
	commands := [][]string{
		{"kind", "create", "cluster", "--name", "goobers-ha-nightly", "--config", "test/ha/kind/kind.yaml", "--image", "kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a", "--wait", "5m"},
		{"kubectl", "apply", "--server-side", "-f", filepath.Join(assets, "calico.yaml")},
		{"kubectl", "rollout", "status", "daemonset/calico-node", "-n", "kube-system", "--timeout=5m"},
		{"kubectl", "wait", "--for=condition=Ready", "pods", "--all", "-A", "--timeout=5m"},
		{"kubectl", "apply", "--server-side", "-f", filepath.Join(assets, "cnpg.yaml")},
		{"kubectl", "rollout", "status", "deployment/cnpg-controller-manager", "-n", "cnpg-system", "--timeout=5m"},
		{"kubectl", "create", "namespace", "goobers-temporal"},
		{"kubectl", "apply", "-f", "test/ha/kind/cnpg-cluster.yaml"},
		{"kubectl", "wait", "--for=condition=Ready", "cluster/temporal-postgres", "-n", "goobers-temporal", "--timeout=10m"},
		{"kind", "load", "docker-image", "goobers:ha-nightly", "--name", "goobers-ha-nightly"},
	}
	for _, args := range commands {
		if err := command(ctx, env, args...); err != nil {
			return err
		}
	}
	for index, step := range temporalLadder {
		if err := runTemporalStep(ctx, env, assets, evidence, step); err != nil {
			return err
		}
		if index == 0 {
			if err := proveSchemaFailureBlocksRollout(ctx, env, assets, temporalLadder[index+1], step); err != nil {
				return err
			}
		}
	}
	if err := toxiproxySmoke(ctx, env, bin, evidence, toxiproxyURL); err != nil {
		return err
	}
	return verifyAndSummarize(ctx, env, evidence)
}

func runTemporalStep(ctx context.Context, env []string, assets, evidence string, step temporalStep) error {
	legacy := step.chart != "temporal-1.3.0.tgz"
	if legacy {
		if err := runLegacySchema(ctx, env, assets, step); err != nil {
			return fmt.Errorf("upgrade temporal %s schema before server rollout: %w", step.server, err)
		}
	}
	args := []string{"helm", "upgrade", "--install", "temporal", filepath.Join(assets, step.chart),
		"--namespace", "goobers-temporal", "--wait", "--timeout", "15m",
		"--set-string", "server.image.tag=" + step.image}
	for _, values := range step.values {
		args = append(args, "--values", values)
	}
	if legacy {
		args = append(args, "--set", "schema.setup.enabled=false", "--set", "schema.update.enabled=false")
	}
	if err := command(ctx, env, args...); err != nil {
		return fmt.Errorf("upgrade temporal %s: %w", step.server, err)
	}
	if err := assertPersistenceConfig(ctx, env, evidence, step.server); err != nil {
		return err
	}
	image, err := output(ctx, env, "kubectl", "get", "deployment/temporal-frontend", "-n", "goobers-temporal",
		"-o", "jsonpath={.spec.template.spec.containers[0].image}")
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.TrimSpace(image), ":"+step.image) {
		return fmt.Errorf("temporal frontend image = %q after %s step", strings.TrimSpace(image), step.server)
	}
	if err := waitForTemporalHealth(ctx, env); err != nil {
		return fmt.Errorf("temporal %s cluster smoke: %w", step.server, err)
	}
	if err := waitForShardSettlement(ctx, env, evidence, step.server); err != nil {
		return err
	}
	if err := command(ctx, env, "kubectl", "delete", "job/goobers-temporal-namespace",
		"-n", "goobers-temporal", "--ignore-not-found"); err != nil {
		return err
	}
	if err := command(ctx, env, "kubectl", "apply", "-k", "deploy/reference/temporal"); err != nil {
		return err
	}
	if err := command(ctx, env, "kubectl", "wait", "--for=condition=Complete", "job/goobers-temporal-namespace",
		"-n", "goobers-temporal", "--timeout=5m"); err != nil {
		return fmt.Errorf("temporal %s namespace registration: %w", step.server, err)
	}
	if err := runUpgradeProbe(ctx, env, evidence, step, step.server == temporalLadder[len(temporalLadder)-1].server); err != nil {
		return err
	}
	if err := command(ctx, env, "kubectl", "delete", "job/goobers-reference-smoke", "-n", "goobers-system", "--ignore-not-found"); err != nil {
		return err
	}
	if err := command(ctx, env, "kubectl", "apply", "-k", "test/ha/kind/reference"); err != nil {
		return err
	}
	if err := command(ctx, env, "kubectl", "wait", "--for=condition=Complete", "job/goobers-reference-smoke",
		"-n", "goobers-system", "--timeout=5m"); err != nil {
		return fmt.Errorf("temporal %s D11 reference smoke: %w", step.server, err)
	}
	return os.WriteFile(filepath.Join(evidence, "temporal-"+step.server+"-smoke.txt"),
		[]byte("schema-first migration, server rollout, cluster health, cross-version workflow continuation, and history replay passed\n"), 0o644)
}

func waitForShardSettlement(ctx context.Context, env []string, evidence, serverVersion string) error {
	query := "SELECT count(*), min(range_id) FROM shards;"
	var last string
	for range 60 {
		result, err := output(ctx, env, "kubectl", "exec", "-n", "goobers-temporal", "temporal-postgres-1",
			"--", "psql", "-U", "postgres", "-d", "temporal", "-Atc", query)
		if err == nil {
			last = strings.TrimSpace(result)
			fields := strings.Split(last, "|")
			if len(fields) == 2 && fields[0] == "4" && fields[1] != "" && fields[1] != "0" {
				return os.WriteFile(filepath.Join(evidence, "temporal-"+serverVersion+"-shards.txt"),
					[]byte("count|min_range_id\n"+last+"\n"), 0o644)
			}
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("temporal %s shards did not settle (last count|min_range_id=%q)", serverVersion, last)
}

func runUpgradeProbe(ctx context.Context, env []string, evidence string, step temporalStep, final bool) error {
	address := envValue(env, "TEMPORAL_UPGRADE_ADDRESS")
	if address == "" {
		return errors.New("TEMPORAL_UPGRADE_ADDRESS is required")
	}
	_, port, ok := strings.Cut(address, ":")
	if !ok || port == "" {
		return fmt.Errorf("TEMPORAL_UPGRADE_ADDRESS %q must be host:port", address)
	}
	logFile, err := os.Create(filepath.Join(evidence, "temporal-"+step.server+"-port-forward.log"))
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	forward := exec.CommandContext(ctx, "kubectl", "port-forward", "-n", "goobers-temporal",
		"service/temporal-frontend", port+":7233", "--address", "127.0.0.1")
	forward.Env, forward.Stdout, forward.Stderr = env, logFile, logFile
	if err := forward.Start(); err != nil {
		return fmt.Errorf("start Temporal port-forward: %w", err)
	}
	defer func() {
		_ = forward.Process.Signal(os.Interrupt)
		_ = forward.Wait()
	}()
	for range 30 {
		connection, dialErr := net.DialTimeout("tcp", address, time.Second)
		if dialErr == nil {
			_ = connection.Close()
			break
		}
		time.Sleep(time.Second)
	}
	args := []string{"run", "./test/ha/temporalprobe", "-address", address, "-server-version", step.server,
		"-state", filepath.Join(evidence, "temporal-upgrade-probe.state"),
		"-evidence", filepath.Join(evidence, "temporal-"+step.server+"-workflow.json")}
	if final {
		args = append(args, "-final")
	}
	probe := exec.CommandContext(ctx, "go", args...)
	probe.Env, probe.Stdout, probe.Stderr = env, os.Stdout, os.Stderr
	if err := probe.Run(); err != nil {
		return fmt.Errorf("temporal %s real workflow probe: %w", step.server, err)
	}
	replay := exec.CommandContext(ctx, "go", "test", "-v", "./internal/engine", "-run",
		`Test(StaticBranchBodiesRealHistoryReplay|FreezeMapRosterHistoryReplaysAtEveryWorkflowTaskStart|JournalOwnerRealHistoriesReplay)$`,
		"-count=1")
	replay.Env = append(env, temporaltest.ServerAddressEnvVar+"="+address)
	output, err := replay.CombinedOutput()
	if writeErr := os.WriteFile(filepath.Join(evidence, "temporal-"+step.server+"-engine-replay.log"), output, 0o644); writeErr != nil {
		return writeErr
	}
	if err != nil {
		return fmt.Errorf("temporal %s engine replay suites: %w", step.server, err)
	}
	if bytes.Count(output, []byte("--- PASS:")) < 3 {
		return fmt.Errorf("temporal %s engine replay suite ran fewer than three required tests", step.server)
	}
	return nil
}

func assertPersistenceConfig(ctx context.Context, env []string, evidence, serverVersion string) error {
	config, err := output(ctx, env, "kubectl", "get", "configmap", "-n", "goobers-temporal",
		"-l", "app.kubernetes.io/instance=temporal", "-o", "yaml")
	if err != nil {
		return err
	}
	for _, want := range []string{"temporal-postgres-rw.goobers-temporal.svc", "temporal_visibility"} {
		if !strings.Contains(config, want) {
			return fmt.Errorf("temporal %s rendered persistence config is missing %q", serverVersion, want)
		}
	}
	return os.WriteFile(filepath.Join(evidence, "temporal-"+serverVersion+"-config.yaml"), []byte(config), 0o644)
}

func runLegacySchema(ctx context.Context, env []string, assets string, step temporalStep) error {
	manifest, job, err := renderLegacySchema(ctx, env, assets, step, "preflight")
	if err != nil {
		return err
	}
	apply := exec.CommandContext(ctx, "kubectl", "apply", "-n", "goobers-temporal", "-f", "-")
	apply.Env, apply.Stdin, apply.Stdout, apply.Stderr = env, strings.NewReader(manifest), os.Stdout, os.Stderr
	if err := apply.Run(); err != nil {
		return fmt.Errorf("apply schema job: %w", err)
	}
	if err := command(ctx, env, "kubectl", "wait", "--for=condition=Complete", "job/"+job,
		"-n", "goobers-temporal", "--timeout=10m"); err != nil {
		return err
	}
	return command(ctx, env, "kubectl", "delete", "job/"+job, "-n", "goobers-temporal")
}

func renderLegacySchema(ctx context.Context, env []string, assets string, step temporalStep, purpose string) (string, string, error) {
	suffix := strings.ReplaceAll(step.server, ".", "-")
	fullname := "temporal-schema-" + purpose + "-" + suffix
	render := exec.CommandContext(ctx, "helm", "template", "temporal", filepath.Join(assets, step.chart),
		"--namespace", "goobers-temporal",
		"--values", step.values[0],
		"--set-string", "server.image.tag="+step.image,
		"--set", "fullnameOverride="+fullname,
		"--show-only", "templates/server-job.yaml")
	render.Env = env
	manifest, err := render.Output()
	if err != nil {
		return "", "", fmt.Errorf("render schema job: %w", err)
	}
	return string(manifest), fullname + "-schema-1", nil
}

func proveSchemaFailureBlocksRollout(ctx context.Context, env []string, assets string, next, current temporalStep) error {
	manifest, job, err := renderLegacySchema(ctx, env, assets, next, "failure-proof")
	if err != nil {
		return err
	}
	manifest = strings.Replace(manifest, "backoffLimit: 100", "backoffLimit: 0", 1)
	manifest = strings.Replace(manifest,
		"command: ['temporal-sql-tool', 'setup-schema', '-v', '0.0']",
		"command: ['sh', '-c', 'exit 42']", 1)
	apply := exec.CommandContext(ctx, "kubectl", "apply", "-n", "goobers-temporal", "-f", "-")
	apply.Env, apply.Stdin, apply.Stdout, apply.Stderr = env, strings.NewReader(manifest), os.Stdout, os.Stderr
	if err := apply.Run(); err != nil {
		return fmt.Errorf("apply schema failure proof: %w", err)
	}
	if err := command(ctx, env, "kubectl", "wait", "--for=condition=Failed", "job/"+job,
		"-n", "goobers-temporal", "--timeout=2m"); err != nil {
		return err
	}
	image, err := output(ctx, env, "kubectl", "get", "deployment/temporal-frontend", "-n", "goobers-temporal",
		"-o", "jsonpath={.spec.template.spec.containers[0].image}")
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.TrimSpace(image), ":"+current.image) {
		return fmt.Errorf("schema failure changed frontend image to %q, want current %q", strings.TrimSpace(image), current.image)
	}
	return command(ctx, env, "kubectl", "delete", "job/"+job, "-n", "goobers-temporal")
}

func waitForTemporalHealth(ctx context.Context, env []string) error {
	args := []string{"kubectl", "exec", "-n", "goobers-temporal", "deployment/temporal-admintools",
		"--", "temporal", "--address", "temporal-frontend:7233", "operator", "cluster", "health"}
	var lastErr error
	for range 30 {
		if lastErr = command(ctx, env, args...); lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), lastErr)
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("frontend did not become healthy after 30 probes: %w", lastErr)
}

func toxiproxySmoke(ctx context.Context, env []string, bin, evidence, endpoint string) error {
	logFile, err := os.Create(filepath.Join(evidence, "toxiproxy.log"))
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	process := exec.CommandContext(ctx, filepath.Join(bin, "toxiproxy-server"), "-host", "127.0.0.1", "-port", "8474")
	process.Env, process.Stdout, process.Stderr = env, logFile, logFile
	if err := process.Start(); err != nil {
		return err
	}
	defer func() {
		_ = process.Process.Signal(os.Interrupt)
		_ = process.Wait()
	}()
	client := http.Client{Timeout: 2 * time.Second}
	for range 30 {
		response, requestErr := client.Get(endpoint)
		if requestErr == nil && response.StatusCode == http.StatusOK {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil {
				return readErr
			}
			return os.WriteFile(filepath.Join(evidence, "toxiproxy-version.txt"), body, 0o644)
		}
		if response != nil {
			_ = response.Body.Close()
		}
		time.Sleep(time.Second)
	}
	return errors.New("toxiproxy did not become ready within 30 seconds")
}

func verifyAndSummarize(ctx context.Context, env []string, evidence string) error {
	networkPolicies, err := output(ctx, env, "kubectl", "get", "networkpolicy", "-A", "-o", "name")
	if err != nil || strings.TrimSpace(networkPolicies) == "" {
		return errors.New("calico environment has no network policies")
	}
	instances, err := output(ctx, env, "kubectl", "get", "cluster", "temporal-postgres", "-n", "goobers-temporal", "-o", "jsonpath={.spec.instances}")
	if err != nil || strings.TrimSpace(instances) != "3" {
		return fmt.Errorf("CNPG instances = %q, want 3", strings.TrimSpace(instances))
	}
	if err := command(ctx, env, "kubectl", "get", "service", "temporal-frontend", "-n", "goobers-temporal"); err != nil {
		return err
	}
	logs, err := output(ctx, env, "kubectl", "logs", "job/goobers-reference-smoke", "-n", "goobers-system")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(evidence, "reference-smoke.log"), []byte(logs), 0o644); err != nil {
		return err
	}
	summary := "### HA nightly kind\n\n" +
		"- Owner rotation: `" + envDefault("HA_NIGHTLY_OWNER_ROTATION", "control-plane-ha-maintainers") + "`\n" +
		"- Calico enforcing CNI: ready\n- CNPG instances: 3\n- Temporal frontend: ready\n" +
		"- Temporal ladder: 1.27.2 -> 1.27.4 -> 1.28.4 -> 1.29.7 -> 1.30.8 -> 1.31.0 (Worker Deployment GA)\n" +
		"- Schema-first migration, cluster workflow continuation, and history replay: passed at every step\n" +
		"- Reference image smoke: passed\n- Toxiproxy: ready\n" +
		"- Benchmarks: not yet registered; future benchmark values are reported, never asserted\n"
	if err := os.WriteFile(filepath.Join(evidence, "summary.md"), []byte(summary), 0o644); err != nil {
		return err
	}
	if stepSummary := os.Getenv("GITHUB_STEP_SUMMARY"); stepSummary != "" {
		file, openErr := os.OpenFile(stepSummary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if openErr != nil {
			return openErr
		}
		defer func() { _ = file.Close() }()
		_, err = file.WriteString(summary)
	}
	return err
}

func collect() error {
	evidence, err := requiredEnv("HA_EVIDENCE_DIR")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(evidence, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := capture(ctx, filepath.Join(evidence, "cluster-info.txt"), "kubectl", "cluster-info"); err != nil {
		return os.WriteFile(filepath.Join(evidence, "cluster-unavailable.txt"), []byte("kind cluster was not created\n"), 0o644)
	}
	for name, args := range map[string][]string{
		"nodes.txt":              {"kubectl", "get", "nodes", "-o", "wide"},
		"pods.txt":               {"kubectl", "get", "pods", "-A", "-o", "wide"},
		"events.txt":             {"kubectl", "get", "events", "-A", "--sort-by=.lastTimestamp"},
		"cnpg.yaml":              {"kubectl", "get", "cluster", "temporal-postgres", "-n", "goobers-temporal", "-o", "yaml"},
		"helm-releases.txt":      {"helm", "list", "-A"},
		"temporal-logs.txt":      {"kubectl", "logs", "-n", "goobers-temporal", "--all-containers", "--prefix", "--tail=200", "-l", "app.kubernetes.io/instance=temporal"},
		"namespace-job-logs.txt": {"kubectl", "logs", "-n", "goobers-temporal", "--all-containers", "--prefix", "--tail=200", "-l", "app.kubernetes.io/component=namespace-registrar"},
	} {
		_ = capture(ctx, filepath.Join(evidence, name), args...)
	}
	return nil
}

func command(ctx context.Context, env []string, args ...string) error {
	fmt.Printf("+ %s\n", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func output(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = env
	data, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.Join(args, " "), err)
	}
	return string(data), nil
}

func capture(ctx context.Context, path string, args ...string) error {
	data, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	if writeErr := os.WriteFile(path, data, 0o644); writeErr != nil {
		return writeErr
	}
	return err
}

func requiredEnv(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envValue(env []string, name string) string {
	prefix := name + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
