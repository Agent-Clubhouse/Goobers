package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

const (
	probeNamespace = "default"
	probeQueue     = "goobers-ha-upgrade-probe"
	probeQuery     = "ready"
	probeSignal    = "release"
)

func main() {
	var address, serverVersion, statePath, evidencePath string
	var final bool
	flag.StringVar(&address, "address", "", "Temporal frontend address")
	flag.StringVar(&serverVersion, "server-version", "", "expected Temporal server version")
	flag.StringVar(&statePath, "state", "", "path carrying the held workflow ID between upgrade steps")
	flag.StringVar(&evidencePath, "evidence", "", "path for the step evidence JSON")
	flag.BoolVar(&final, "final", false, "complete and replay the final step instead of leaving it held")
	flag.Parse()
	if address == "" || serverVersion == "" || statePath == "" || evidencePath == "" {
		exitf("address, server-version, state, and evidence are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, err := client.Dial(client.Options{HostPort: address, Namespace: probeNamespace})
	if err != nil {
		exitf("dial Temporal: %v", err)
	}
	defer c.Close()
	info, err := c.WorkflowService().GetSystemInfo(ctx, &workflowservice.GetSystemInfoRequest{})
	if err != nil {
		exitf("get Temporal system info: %v", err)
	}
	if info.GetServerVersion() != serverVersion {
		exitf("Temporal server version = %q, want %q", info.GetServerVersion(), serverVersion)
	}
	w := temporalworker.New(c, probeQueue, temporalworker.Options{})
	w.RegisterWorkflow(upgradeProbeWorkflow)
	if err := w.Start(); err != nil {
		exitf("start probe worker: %v", err)
	}
	defer w.Stop()

	evidence := probeEvidence{ServerVersion: serverVersion}
	if previous, err := os.ReadFile(statePath); err == nil {
		evidence.ContinuedWorkflow = strings.TrimSpace(string(previous))
		evidence.ContinuedEvents = completeAndReplay(ctx, c, evidence.ContinuedWorkflow, "after-"+serverVersion)
	} else if !os.IsNotExist(err) {
		exitf("read probe state: %v", err)
	}

	nextID := "ha-upgrade-" + strings.ReplaceAll(serverVersion, ".", "-")
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: nextID, TaskQueue: probeQueue}, upgradeProbeWorkflow, serverVersion)
	if err != nil {
		exitf("start probe workflow: %v", err)
	}
	waitReady(ctx, c, run.GetID(), run.GetRunID())
	evidence.StartedWorkflow = run.GetID()
	if final {
		evidence.FinalEvents = completeAndReplay(ctx, c, run.GetID(), "final")
		if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
			exitf("remove probe state: %v", err)
		}
	} else if err := os.WriteFile(statePath, []byte(run.GetID()+"\n"), 0o644); err != nil {
		exitf("write probe state: %v", err)
	}
	data, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		exitf("encode evidence: %v", err)
	}
	if err := os.WriteFile(evidencePath, append(data, '\n'), 0o644); err != nil {
		exitf("write evidence: %v", err)
	}
}

type probeEvidence struct {
	ServerVersion     string `json:"server_version"`
	ContinuedWorkflow string `json:"continued_workflow,omitempty"`
	ContinuedEvents   int    `json:"continued_events,omitempty"`
	StartedWorkflow   string `json:"started_workflow"`
	FinalEvents       int    `json:"final_events,omitempty"`
}

func upgradeProbeWorkflow(ctx workflow.Context, startedOn string) (string, error) {
	if err := workflow.SetQueryHandler(ctx, probeQuery, func() (bool, error) { return true, nil }); err != nil {
		return "", err
	}
	var releasedOn string
	workflow.GetSignalChannel(ctx, probeSignal).Receive(ctx, &releasedOn)
	return startedOn + "->" + releasedOn, nil
}

func waitReady(ctx context.Context, c client.Client, workflowID, runID string) {
	for {
		response, err := c.QueryWorkflow(ctx, workflowID, runID, probeQuery)
		if err == nil {
			var ready bool
			if err := response.Get(&ready); err == nil && ready {
				return
			}
		}
		select {
		case <-ctx.Done():
			exitf("wait for workflow %s readiness: %v", workflowID, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

func completeAndReplay(ctx context.Context, c client.Client, workflowID, release string) int {
	if err := c.SignalWorkflow(ctx, workflowID, "", probeSignal, release); err != nil {
		exitf("signal workflow %s: %v", workflowID, err)
	}
	var result string
	run := c.GetWorkflow(ctx, workflowID, "")
	if err := run.Get(ctx, &result); err != nil {
		exitf("complete workflow %s: %v", workflowID, err)
	}
	history := &historypb.History{}
	iter := c.GetWorkflowHistory(ctx, workflowID, run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			exitf("read workflow %s history: %v", workflowID, err)
		}
		history.Events = append(history.Events, event)
	}
	if len(history.Events) == 0 {
		exitf("workflow %s returned an empty history", workflowID)
	}
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(upgradeProbeWorkflow)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		exitf("replay workflow %s: %v", workflowID, err)
	}
	return len(history.Events)
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
