package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

const completedRunPageSize = 100

type completedRunClient interface {
	projectionQuerier
	ListWorkflow(context.Context, *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error)
}

// ProjectionObserver receives the terminal journal sequence after a projection
// is durably published.
type ProjectionObserver func(context.Context, string, uint64) error

// CompletedRunReconciler projects closed Temporal engine runs into their
// gaggle's standard journal root. Reconcile is bounded to one visibility page;
// successive calls continue pagination and cycle back to the newest page.
type CompletedRunReconciler struct {
	client        completedRunClient
	namespace     string
	runsDirs      map[string]string
	observe       ProjectionObserver
	nextPageToken []byte
}

// NewCompletedRunReconciler constructs a reconciler scoped to configured
// gaggle names and their journal roots.
func NewCompletedRunReconciler(c completedRunClient, namespace string, runsDirs map[string]string, observe ProjectionObserver) (*CompletedRunReconciler, error) {
	if c == nil {
		return nil, errors.New("engine: Temporal client is required")
	}
	if namespace == "" {
		return nil, errors.New("engine: Temporal namespace is required")
	}
	if len(runsDirs) == 0 {
		return nil, errors.New("engine: at least one gaggle runs directory is required")
	}
	return &CompletedRunReconciler{client: c, namespace: namespace, runsDirs: runsDirs, observe: observe}, nil
}

// Reconcile processes one bounded page of closed workflow executions.
func (r *CompletedRunReconciler) Reconcile(ctx context.Context) (int, error) {
	response, err := r.client.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Namespace:     r.namespace,
		PageSize:      completedRunPageSize,
		NextPageToken: r.nextPageToken,
		Query:         "ExecutionStatus != 'Running'",
	})
	if err != nil {
		return 0, fmt.Errorf("engine: list completed workflows: %w", err)
	}
	r.nextPageToken = append(r.nextPageToken[:0], response.NextPageToken...)

	var (
		projected int
		errs      []error
	)
	for _, info := range response.Executions {
		memo := info.GetMemo().GetFields()[RunGaggleMemoKey]
		if memo == nil {
			continue
		}
		var gaggle string
		if err := converter.GetDefaultDataConverter().FromPayload(memo, &gaggle); err != nil {
			errs = append(errs, fmt.Errorf("engine: decode gaggle memo for %q: %w", info.GetExecution().GetWorkflowId(), err))
			continue
		}
		runsDir, ok := r.runsDirs[gaggle]
		if !ok {
			continue
		}
		runID := info.GetExecution().GetWorkflowId()
		if runID == "" {
			errs = append(errs, errors.New("engine: completed workflow has no workflow id"))
			continue
		}
		dir, err := completedRunDir(runsDir, runID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if journal.Recorded(dir) {
			complete, err := projectedJournalComplete(dir)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if complete {
				if err := observeProjectedRun(ctx, r.observe, runID, dir); err != nil {
					errs = append(errs, err)
				}
				continue
			}
		}
		if _, err := projectCompletedRun(ctx, r.client, runID, gaggle, runsDir, r.observe); err != nil {
			errs = append(errs, err)
			continue
		}
		projected++
	}
	return projected, errors.Join(errs...)
}

// ProjectCompletedRunForGaggle is the manual projection path with the same
// gaggle validation and read-model notification used by the reconciler.
func ProjectCompletedRunForGaggle(ctx context.Context, q projectionQuerier, workflowID, gaggle, runsDir string, observe ProjectionObserver) (string, error) {
	dir, err := completedRunDir(runsDir, workflowID)
	if err != nil {
		return "", err
	}
	if journal.Recorded(dir) {
		complete, err := projectedJournalComplete(dir)
		if err != nil {
			return "", err
		}
		if complete {
			return dir, observeProjectedRun(ctx, observe, workflowID, dir)
		}
	}
	return projectCompletedRun(ctx, q, workflowID, gaggle, runsDir, observe)
}

func completedRunDir(runsDir, workflowID string) (string, error) {
	if !apiv1.ValidRunID(workflowID) {
		return "", fmt.Errorf("%w: workflow id %q is not a safe path segment", ErrUnprojectable, workflowID)
	}
	return filepath.Join(runsDir, workflowID), nil
}

func projectCompletedRun(ctx context.Context, q projectionQuerier, workflowID, gaggle, runsDir string, observe ProjectionObserver) (string, error) {
	proj, err := queryProjection(ctx, q, workflowID)
	if err != nil {
		return "", err
	}
	if proj.Identity.RunID != workflowID {
		return "", fmt.Errorf("%w: workflow id %q projected run id %q", ErrUnprojectable, workflowID, proj.Identity.RunID)
	}
	if proj.Identity.Gaggle != gaggle {
		return "", fmt.Errorf("%w: workflow %q memo gaggle %q does not match projected gaggle %q", ErrUnprojectable, workflowID, gaggle, proj.Identity.Gaggle)
	}
	dir, err := ProjectCompletedRun(ctx, q, workflowID, runsDir)
	if err != nil {
		return "", err
	}
	if err := observeProjectedRun(ctx, observe, workflowID, dir); err != nil {
		return "", err
	}
	return dir, nil
}

func observeProjectedRun(ctx context.Context, observe ProjectionObserver, workflowID, dir string) error {
	if observe == nil {
		return nil
	}
	rd, err := journal.OpenRead(dir)
	if err != nil {
		return err
	}
	events, err := rd.Events()
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return fmt.Errorf("%w: projected journal has no events", ErrUnprojectable)
	}
	if err := observe(ctx, workflowID, events[len(events)-1].Seq); err != nil {
		return fmt.Errorf("engine: notify projected run %q: %w", workflowID, err)
	}
	return nil
}
