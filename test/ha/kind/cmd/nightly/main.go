package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

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
	env := append(os.Environ(), "PATH="+path)
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
		{"helm", "upgrade", "--install", "temporal", filepath.Join(assets, "temporal.tgz"), "--namespace", "goobers-temporal", "--values", "deploy/reference/temporal/values.yaml", "--values", "test/ha/kind/temporal-values.yaml", "--wait", "--timeout", "15m"},
		{"kind", "load", "docker-image", "goobers:ha-nightly", "--name", "goobers-ha-nightly"},
		{"kubectl", "apply", "-k", "test/ha/kind/reference"},
		{"kubectl", "wait", "--for=condition=Complete", "job/goobers-reference-smoke", "-n", "goobers-system", "--timeout=5m"},
	}
	for _, args := range commands {
		if err := command(ctx, env, args...); err != nil {
			return err
		}
	}
	if err := toxiproxySmoke(ctx, env, bin, evidence, toxiproxyURL); err != nil {
		return err
	}
	return verifyAndSummarize(ctx, env, evidence)
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
		"nodes.txt":         {"kubectl", "get", "nodes", "-o", "wide"},
		"pods.txt":          {"kubectl", "get", "pods", "-A", "-o", "wide"},
		"events.txt":        {"kubectl", "get", "events", "-A", "--sort-by=.lastTimestamp"},
		"cnpg.yaml":         {"kubectl", "get", "cluster", "temporal-postgres", "-n", "goobers-temporal", "-o", "yaml"},
		"helm-releases.txt": {"helm", "list", "-A"},
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

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
