package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workerrouting"
)

func stubStatusEngineRouting(t *testing.T, result workerrouting.Assessment) *[]workerrouting.Target {
	t.Helper()
	var probed []workerrouting.Target
	previous := probeStatusEngineRouting
	probeStatusEngineRouting = func(ctx context.Context, target workerrouting.Target, now time.Time) workerrouting.Assessment {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("engine routing probe ran without a deadline")
		}
		probed = append(probed, target)
		return result
	}
	t.Cleanup(func() { probeStatusEngineRouting = previous })
	return &probed
}

func enableStatusEngine(t *testing.T, root string) {
	t.Helper()
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Engine = &instance.EngineConfig{HostPort: "temporal-frontend:7233", Namespace: "goobers-ns", TaskQueue: "engine-q"}
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
}

var stalledStatusRouting = workerrouting.Assessment{
	State: workerrouting.StateStalled, Deployment: "goobers", TaskQueue: "engine-q", CurrentVersion: "__unversioned__",
	Workers:     []workerrouting.WorkerRouting{{Version: "goobers.v2", Pollers: 1, Routing: workerrouting.WorkerUnrouted}},
	Detail:      "no live worker on task queue \"engine-q\" is routed workflow tasks",
	Remediation: []string{"temporal worker deployment set-current-version --deployment-name goobers --build-id v2"},
}

// #6020: a stalled cloud engine (Ready worker, no poll errors, no dispatch)
// is reported by status, with the command that unstalls it.
func TestStatusReportsStalledEngineRouting(t *testing.T) {
	root := initScheduledDemo(t)
	enableStatusEngine(t, root)
	probed := stubStatusEngineRouting(t, stalledStatusRouting)

	code, stdout, stderr := runArgs(t, "status", "--json", root)
	if code != 0 {
		t.Fatalf("status --json: code = %d, stderr = %q", code, stderr)
	}
	var got statusJSONOutput
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("status JSON = %q: %v", stdout, err)
	}
	if got.EngineRouting == nil || got.EngineRouting.State != workerrouting.StateStalled ||
		len(got.EngineRouting.Remediation) != 1 || !strings.Contains(stdout, `"engineRouting":{"state":"stalled"`) {
		t.Fatalf("engineRouting = %+v in %s", got.EngineRouting, stdout)
	}
	want := workerrouting.Target{HostPort: "temporal-frontend:7233", Namespace: "goobers-ns", TaskQueue: "engine-q"}
	if len(*probed) != 1 || (*probed)[0] != want {
		t.Fatalf("probed %+v, want one probe of %+v", *probed, want)
	}

	code, stdout, stderr = runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	for _, line := range []string{
		"Warning: engine routing stalled: no live worker",
		"Run: temporal worker deployment set-current-version --deployment-name goobers --build-id v2",
	} {
		if !strings.Contains(stdout, line) {
			t.Fatalf("status text lacks %q:\n%s", line, stdout)
		}
	}
}

// When the probe cannot see the routing, status says unknown rather than
// staying silent as if it were healthy.
func TestStatusReportsUnknownEngineRouting(t *testing.T) {
	root := initScheduledDemo(t)
	enableStatusEngine(t, root)
	stubStatusEngineRouting(t, workerrouting.Unknown("engine-q", "cannot reach Temporal frontend temporal-frontend:7233", time.Now()))
	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "Engine routing: unknown (cannot reach Temporal frontend temporal-frontend:7233)") {
		t.Fatalf("status text lacks the unknown routing line:\n%s", stdout)
	}
}

// A local instance has no engine: no probe, no field, no line.
func TestStatusSkipsEngineRoutingWithoutEngine(t *testing.T) {
	root := initScheduledDemo(t)
	probed := stubStatusEngineRouting(t, stalledStatusRouting)
	code, stdout, stderr := runArgs(t, "status", "--json", root)
	if code != 0 {
		t.Fatalf("status --json: code = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(stdout, "engineRouting") {
		t.Fatalf("local instance status JSON carries engineRouting: %s", stdout)
	}
	if code, stdout, _ = runArgs(t, "status", root); code != 0 || strings.Contains(stdout, "ngine routing") {
		t.Fatalf("local instance status text = %d %q", code, stdout)
	}
	if len(*probed) != 0 {
		t.Fatalf("probed Temporal %d time(s) for an instance with no engine", len(*probed))
	}
}

func TestStatusEngineRoutingRefreshWindow(t *testing.T) {
	probed := stubStatusEngineRouting(t, stalledStatusRouting)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	routing := &statusEngineRouting{now: func() time.Time { return now }}
	routing.Load(context.Background())
	now = now.Add(statusEngineRoutingRefresh - time.Second)
	routing.Load(context.Background())
	if len(*probed) != 1 {
		t.Fatalf("probed %d time(s) within the refresh window, want 1", len(*probed))
	}
	now = now.Add(time.Second)
	if got := routing.Load(context.Background()); got == nil || got.State != workerrouting.StateStalled || len(*probed) != 2 {
		t.Fatalf("after the window: %+v, %d probe(s)", got, len(*probed))
	}
}
