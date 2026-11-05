// Package child runs generated-child qualification against the kind reference
// install. It observes PostgreSQL custody, Temporal execution and Kubernetes
// pod identities; it never supplies an engine handoff or replaces an executor.
package child

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	corev1 "k8s.io/api/core/v1"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/test/ha/chaos"
	"github.com/goobers/goobers/test/ha/childscenarios"
)

const happyPath = "CHILD-HAPPY"

type lineage struct {
	ParentRunID, ParentExecution, ParentTemporalRunID string
	ChildID, ChildRunID, State                        string
	AdmittedChildren, ChargedAttempts                 int
}

type fixture struct {
	exec       chaos.Executor
	temporal   client.Client
	workflow   string
	lineage    lineage
	pod        corev1.Pod
	before     childscenarios.ChildFamilyObservation
	after      childscenarios.ChildFamilyObservation
	evicted    bool
	ownerAfter string
}

// Run writes evidence even when a scenario fails. The caller has already
// applied the reference overlay and forwarded the real Temporal frontend.
func Run(ctx context.Context, exec chaos.Executor, address, evidence string) error {
	c, err := client.DialContext(ctx, client.Options{HostPort: address, Namespace: "kind-child"})
	if err != nil {
		summary := chaos.Summary{Results: []chaos.Result{
			{ID: happyPath, Status: chaos.Failed, Reason: err.Error()},
			{ID: childscenarios.Chaos13Child, Status: chaos.Failed, Reason: err.Error()},
		}}
		return errors.Join(err, writeJSON(filepath.Join(evidence, "child-chaos-summary.json"), summary))
	}
	defer c.Close()
	var results []chaos.Result
	var failures []error
	for _, mode := range []string{"happy", "failover"} {
		f := &fixture{exec: exec, temporal: c, workflow: "kind-child-" + mode}
		id := happyPath
		start := time.Now()
		if mode == "failover" {
			id = childscenarios.Chaos13Child
			err = childscenarios.RunCHAOS13Child(ctx, f)
		} else {
			var before childscenarios.ChildFamilyObservation
			before, err = f.StartChildMidStage(ctx)
			if err == nil {
				var after childscenarios.ChildFamilyObservation
				after, err = f.WaitForChildFamily(ctx, before.ParentRunID)
				if err == nil && (after.ChildID != before.ChildID || after.ChildTemporalRunID != before.ChildTemporalRunID || after.AdmittedChildren != 1 || after.ChargedAttempts != 1 || !after.ChildTerminal || !after.ParentTerminal) {
					err = fmt.Errorf("happy child family differs: %+v", after)
				}
			}
		}
		result := chaos.Result{ID: id, Status: chaos.Passed, Duration: time.Since(start).String()}
		if err != nil {
			result.Status, result.Reason = chaos.Failed, err.Error()
			failures = append(failures, err)
		}
		results = append(results, result)
		if err := writeJSON(filepath.Join(evidence, id+".json"), struct {
			Before, After childscenarios.ChildFamilyObservation
		}{f.before, f.after}); err != nil {
			failures = append(failures, err)
		}
	}
	if err := writeJSON(filepath.Join(evidence, "child-chaos-summary.json"), chaos.Summary{Results: results}); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func (f *fixture) StartChildMidStage(ctx context.Context) (childscenarios.ChildFamilyObservation, error) {
	err := poll(ctx, func() (bool, error) {
		rows, err := f.readLineage(ctx)
		if err != nil || len(rows) == 0 {
			return false, err
		}
		if len(rows) != 1 || rows[0].AdmittedChildren != 1 {
			return false, fmt.Errorf("expected exactly one generated child for %s", f.workflow)
		}
		f.lineage = rows[0]
		if f.lineage.State == "failed" || f.lineage.State == "cancelled" {
			return false, fmt.Errorf("child settled before side effect: %s", f.lineage.State)
		}
		pods, err := f.pods(ctx, "gaggle-reference", "goobers.dev/role=stage")
		if err != nil {
			return false, err
		}
		for _, pod := range pods.Items {
			if pod.Annotations[dispatcher.AnnotationRunID] != f.lineage.ChildRunID || pod.Annotations[dispatcher.AnnotationStage] != "effect" || pod.Status.Phase != corev1.PodRunning {
				continue
			}
			f.pod = pod
			if _, err := f.exec.Run(ctx, "", "kubectl", "-n", "gaggle-reference", "exec", pod.Name, "-c", "stage", "--", "sh", "-ec", "test -f /tmp/kind-child-ready && test $(wc -l < /tmp/kind-child-effects) -eq 1"); err != nil {
				return false, nil // pod exists before its real command reaches the barrier
			}
			owner, err := f.custodyOwner(ctx)
			if err != nil || owner == "" {
				return false, err
			}
			execution, err := f.childExecution(ctx)
			if err != nil {
				return false, err
			}
			described, err := f.temporal.DescribeWorkflowExecution(ctx, execution.WorkflowId, execution.RunId)
			if err != nil {
				return false, err
			}
			attempt, err := strconv.Atoi(pod.Labels[dispatcher.LabelAttempt])
			if err != nil {
				return false, err
			}
			f.before = childscenarios.ChildFamilyObservation{ParentRunID: f.lineage.ParentRunID, ChildID: f.lineage.ChildID,
				ChildTemporalRunID: described.WorkflowExecutionInfo.Execution.RunId, Attempt: attempt, StagePodUID: string(pod.UID),
				OwnerWorker: owner, AdmittedChildren: f.lineage.AdmittedChildren, ChargedAttempts: f.lineage.ChargedAttempts}
			if f.before.ChargedAttempts > 1 {
				return false, errors.New("child charged more than one attempt before eviction")
			}
			return f.before.ChargedAttempts == 1, nil
		}
		return false, nil
	})
	return f.before, err
}

func (f *fixture) EvictOwningWorker(ctx context.Context, before childscenarios.ChildFamilyObservation) error {
	pods, err := f.pods(ctx, "goobers-cloud", "goobers.dev/role=worker")
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if string(pod.UID) != before.OwnerWorker {
			continue
		}
		// Delete only the observed worker UID, with an API precondition. Forceful
		// removal exercises heartbeat recovery instead of graceful draining.
		options, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "gracePeriodSeconds": 0, "preconditions": map[string]string{"uid": string(pod.UID)}})
		if err != nil {
			return err
		}
		_, err = f.exec.Run(ctx, string(options), "kubectl", "delete", "--raw", "/api/v1/namespaces/goobers-cloud/pods/"+pod.Name, "-f", "-")
		if err != nil {
			return err
		}
		f.evicted = true
		return poll(ctx, func() (bool, error) {
			owner, err := f.custodyOwner(ctx)
			if err != nil || owner == "" || owner == before.OwnerWorker {
				return false, err
			}
			f.ownerAfter = owner
			return true, nil
		})
	}
	return errors.New("observed owning worker UID disappeared before eviction")
}

func (f *fixture) WaitForChildFamily(ctx context.Context, parent string) (childscenarios.ChildFamilyObservation, error) {
	if parent != f.before.ParentRunID {
		return f.after, errors.New("parent identity changed")
	}
	// Re-read the exact UID and side-effect count AFTER adoption and BEFORE
	// releasing the stage; disposal after surrender must not hide replacement.
	var current corev1.Pod
	raw, err := f.exec.Run(ctx, "", "kubectl", "-n", "gaggle-reference", "get", "pod", f.pod.Name, "-o", "json")
	if err != nil {
		return f.after, err
	}
	if err := json.Unmarshal([]byte(raw), &current); err != nil {
		return f.after, err
	}
	if current.UID != f.pod.UID {
		return f.after, errors.New("child stage pod was replaced")
	}
	if _, err := f.exec.Run(ctx, "", "kubectl", "-n", "gaggle-reference", "exec", current.Name, "-c", "stage", "--", "sh", "-ec", "test $(wc -l < /tmp/kind-child-effects) -eq 1 && touch /tmp/kind-child-release"); err != nil {
		return f.after, err
	}
	var result engine.RunResult
	if err := f.temporal.GetWorkflow(ctx, f.lineage.ParentExecution, f.lineage.ParentTemporalRunID).Get(ctx, &result); err != nil {
		return f.after, err
	}
	if result.Status != engine.StatusCompleted || result.Outputs["parent"].Summary != "kind parent consumed child receipt" {
		return f.after, fmt.Errorf("parent did not consume child receipt: status=%s", result.Status)
	}
	rows, err := f.readLineage(ctx)
	if err != nil {
		return f.after, err
	}
	if len(rows) != 1 {
		return f.after, fmt.Errorf("terminal child lineage count=%d", len(rows))
	}
	execution, err := f.childExecution(ctx)
	if err != nil {
		return f.after, err
	}
	described, err := f.temporal.DescribeWorkflowExecution(ctx, execution.WorkflowId, execution.RunId)
	if err != nil {
		return f.after, err
	}
	attempt, err := strconv.Atoi(current.Labels[dispatcher.LabelAttempt])
	if err != nil {
		return f.after, err
	}
	f.after = f.before
	f.after.Attempt, f.after.StagePodUID = attempt, string(current.UID)
	f.after.ChildID = rows[0].ChildID
	f.after.ChildTemporalRunID = described.WorkflowExecutionInfo.Execution.RunId
	f.after.AdmittedChildren, f.after.ChargedAttempts = rows[0].AdmittedChildren, rows[0].ChargedAttempts
	f.after.ChildTerminal = rows[0].State == "completed" && described.WorkflowExecutionInfo.Status == enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED
	f.after.ParentTerminal = true
	f.after.Adopted = f.evicted && f.ownerAfter != "" && f.ownerAfter != f.before.OwnerWorker
	if f.evicted {
		f.after.OwnerWorker = f.ownerAfter
	}
	return f.after, nil
}

// childExecution reads the actual Temporal child-start event instead of
// assuming the business run ID is also its Temporal workflow ID.
func (f *fixture) childExecution(ctx context.Context) (*commonpb.WorkflowExecution, error) {
	history := f.temporal.GetWorkflowHistory(ctx, f.lineage.ParentExecution, f.lineage.ParentTemporalRunID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	var execution *commonpb.WorkflowExecution
	for history.HasNext() {
		event, err := history.Next()
		if err != nil {
			return nil, err
		}
		started := event.GetChildWorkflowExecutionStartedEventAttributes()
		if started == nil {
			continue
		}
		if execution != nil {
			return nil, errors.New("parent started more than one Temporal child")
		}
		execution = started.WorkflowExecution
	}
	if execution == nil {
		return nil, errors.New("parent has no recorded Temporal child start")
	}
	childHistory := f.temporal.GetWorkflowHistory(ctx, execution.WorkflowId, execution.RunId, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	if !childHistory.HasNext() {
		return nil, errors.New("child has no execution history")
	}
	event, err := childHistory.Next()
	if err != nil {
		return nil, err
	}
	started := event.GetWorkflowExecutionStartedEventAttributes()
	var input engine.RunInput
	if started == nil {
		return nil, errors.New("child history lacks start input")
	}
	if err := converter.GetDefaultDataConverter().FromPayloads(started.Input, &input); err != nil {
		return nil, err
	}
	if input.RunID != f.lineage.ChildRunID || input.FrozenChild == nil {
		return nil, errors.New("temporal child does not match retained lineage")
	}
	return execution, nil
}

func (f *fixture) pods(ctx context.Context, namespace, selector string) (corev1.PodList, error) {
	var pods corev1.PodList
	raw, err := f.exec.Run(ctx, "", "kubectl", "-n", namespace, "get", "pods", "-l", selector, "-o", "json")
	if err == nil {
		err = json.Unmarshal([]byte(raw), &pods)
	}
	return pods, err
}

func (f *fixture) custodyOwner(ctx context.Context) (string, error) {
	workflowID := f.pod.Annotations[dispatcher.AnnotationOwningWorkflowID]
	if workflowID == "" {
		return "", errors.New("stage pod lacks owning workflow identity")
	}
	described, err := f.temporal.DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		return "", err
	}
	workers, err := f.pods(ctx, "goobers-cloud", "goobers.dev/role=worker")
	if err != nil {
		return "", err
	}
	for _, activity := range described.PendingActivities {
		var heartbeat struct {
			UID, Pod, Namespace string
			Custody             bool
		}
		if activity.HeartbeatDetails == nil || converter.GetDefaultDataConverter().FromPayloads(activity.HeartbeatDetails, &heartbeat) != nil || !heartbeat.Custody || heartbeat.UID != string(f.pod.UID) || heartbeat.Pod != f.pod.Name || heartbeat.Namespace != "gaggle-reference" {
			continue
		}
		for _, worker := range workers.Items {
			_, hostPID, _ := strings.Cut(activity.LastWorkerIdentity, "@")
			host, _, _ := strings.Cut(hostPID, "#")
			if worker.DeletionTimestamp == nil && host == worker.Name {
				return string(worker.UID), nil
			}
		}
	}
	return "", nil
}

func (f *fixture) readLineage(ctx context.Context) ([]lineage, error) {
	primary, err := f.exec.Run(ctx, "", "kubectl", "-n", "goobers-temporal", "get", "cluster", "temporal-postgres", "-o", "jsonpath={.status.currentPrimary}")
	if err != nil {
		return nil, err
	}
	// Read-only observations through CNPG's local socket: no database password
	// enters arguments, evidence, the model, workers or stage pods.
	query := `SELECT COALESCE(json_agg(x),'[]') FROM (
SELECT c.parent_run_id AS "ParentRunID", c.child_id AS "ChildID", a.run_id AS "ChildRunID", c.state AS "State",
s.parent_workflow_id AS "ParentExecution", s.parent_temporal_run_id AS "ParentTemporalRunID",
(SELECT count(*) FROM control.child_lineage sibling WHERE sibling.instance_id=c.instance_id AND sibling.parent_run_id=c.parent_run_id AND sibling.occurrence=c.occurrence) AS "AdmittedChildren",
(SELECT count(*) FROM journal.run_event e WHERE e.instance_id=c.instance_id AND e.run_id=a.run_id AND e.stage='effect' AND e.type='stage.started' AND COALESCE(convert_from(e.body,'UTF8')::jsonb->>'attemptClass','policy')<>'infra') AS "ChargedAttempts"
FROM control.child_lineage c JOIN control.acceptance a ON a.instance_id=c.instance_id AND a.id=c.acceptance_id
JOIN control.child_engine_start s ON s.instance_id=c.instance_id AND s.child_id=c.child_id
JOIN control.run_pin p ON p.instance_id=c.instance_id AND p.run_id=c.parent_run_id
WHERE c.instance_id='nightly' AND p.workflow='` + strings.ReplaceAll(f.workflow, "'", "''") + `') x`
	raw, err := f.exec.Run(ctx, "", "kubectl", "-n", "goobers-temporal", "exec", strings.TrimSpace(primary), "-c", "postgres", "--", "psql", "-XAt", "-v", "ON_ERROR_STOP=1", "-d", "goobers_nightly", "-c", query)
	var rows []lineage
	if err == nil {
		err = json.Unmarshal([]byte(raw), &rows)
	}
	return rows, err
}

func poll(ctx context.Context, observe func() (bool, error)) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		ready, err := observe()
		if err != nil || ready {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for durable child boundary: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
