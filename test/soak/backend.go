package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

//go:embed fixtures/*
var fixtures embed.FS

type cliBackend struct {
	binary, root, endpoint string
	client                 *http.Client
	daemon, pressure       *child
	decisions              *os.File
}

func initialize(ctx context.Context, binary, root string, p Profile) error {
	cmd := exec.CommandContext(ctx, binary, "init", "--demo", root)
	cmd.Env = cleanEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("init --demo: %w: %s", err, output)
	}
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return err
	}
	cfg.API.Listen = "127.0.0.1:0"
	cfg.RunConditions.MaxParallelRuns = p.Runs
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		return err
	}
	profile, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "profile.json"), profile, 0o600); err != nil {
		return err
	}
	return installFixtures(root, p)
}

func installFixtures(root string, p Profile) error {
	script, err := fixtures.ReadFile("fixtures/churn.sh")
	if err != nil {
		return err
	}
	scriptPath := filepath.Join(root, "soak-fixture.sh")
	if err := os.WriteFile(scriptPath, script, 0o700); err != nil {
		return err
	}
	data, err := fixtures.ReadFile("fixtures/workflow.yaml")
	if err != nil {
		return err
	}
	var workflow apiv1.Workflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return err
	}
	workflow.Spec.Readiness.MaxConcurrentRuns = int32(p.Runs)
	dir := filepath.Join(root, "config", "gaggles", "demo", "workflows")
	if err := os.Remove(filepath.Join(dir, "demo.yaml")); err != nil {
		return err
	}
	for _, name := range []string{"soak", "soak-failure"} {
		workflow.Name = name
		mode := "success"
		if name == "soak-failure" {
			mode = "failure"
		}
		workflow.Spec.Tasks[0].Run.Command = []string{"sh", scriptPath, mode}
		data, err := yaml.Marshal(workflow)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// Remove ambient endpoint/auth settings so no submission can escape this fixture.
func cleanEnv() []string {
	var env []string
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "GOOBERS_") || strings.HasPrefix(value, "GITHUB_") || strings.HasPrefix(value, "GH_TOKEN=") {
			continue
		}
		env = append(env, value)
	}
	return env
}

func (b *cliBackend) Submit(ctx context.Context, failure bool) (string, error) {
	workflow := "demo/soak"
	if failure {
		workflow = "demo/soak-failure"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.binary, "run", "--force", "--no-wait", workflow, b.root)
	cmd.Env = cleanEnv()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("goobers run: %w: %s", err, output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "accepted" && fields[1] == "trigger" {
			return fields[2], nil
		}
	}
	return "", fmt.Errorf("goobers run returned no durable acceptance identity: %s", output)
}

func (b *cliBackend) Resolve(ctx context.Context, id string) (apicontract.TriggerStatusResponse, error) {
	var status apicontract.TriggerStatusResponse
	err := b.get(ctx, strings.Replace(apicontract.TriggerStatusPath, "{acceptance}", url.PathEscape(id), 1), nil, &status)
	return status, err
}

// List traverses the public paginated RunList query seam, never raw journals.
// Since/Until use the documented start-time axis; FinishedAt is used separately
// by the driver for throughput, so runs started during ramp are not lost.
func (b *cliBackend) List(ctx context.Context, o readservice.RunListOptions) ([]readservice.RunSummary, error) {
	q := url.Values{"gaggle": {o.Gaggle}, "workflow": {o.Workflow}, "phase": {string(o.Phase)}, "since": {o.Since.Format(time.RFC3339Nano)}, "until": {o.Until.Format(time.RFC3339Nano)}, "showNoWork": {"true"}, "limit": {"100"}}
	var runs []readservice.RunSummary
	visited := map[string]bool{}
	for {
		var page readservice.RunList
		if err := b.get(ctx, apicontract.RunsPath, q, &page); err != nil {
			return nil, err
		}
		if page.ReadState != nil && page.ReadState.Completeness == readmodel.CompletenessPartial {
			return nil, fmt.Errorf("run observation is partial")
		}
		runs = append(runs, page.Runs...)
		if page.NextCursor == "" {
			return runs, nil
		}
		if visited[page.NextCursor] {
			return nil, fmt.Errorf("run observation repeated cursor")
		}
		visited[page.NextCursor] = true
		q.Set("cursor", page.NextCursor)
	}
}

func (b *cliBackend) get(ctx context.Context, path string, q url.Values, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.endpoint+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	response, err := b.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("observation HTTP %d for %s", response.StatusCode, path)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(out)
}

func (b *cliBackend) Health() (invalidReason, error) {
	if b.daemon.exited() {
		return daemonHealthFailed, fmt.Errorf("daemon exited; see daemon.log")
	}
	if b.pressure.exited() {
		return loadInjectorCrashed, fmt.Errorf("injector exited; see pressure.log")
	}
	return "", nil
}

func (b *cliBackend) Record(d decision) error {
	if err := json.NewEncoder(b.decisions).Encode(d); err != nil {
		return err
	}
	return b.decisions.Sync()
}
