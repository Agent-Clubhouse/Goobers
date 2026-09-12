package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

const maxStatusQueueWorkflows = 16

func optionalStatusQueueEvidence(enabled bool, sources readservice.LocalSources, workflows []apiv1.Workflow, gaggle, workflow string) *statusQueueEvidence {
	if !enabled {
		return nil
	}
	result := loadStatusQueueEvidence(context.Background(), sources, workflows, gaggle, workflow)
	return &result
}

func statusQueueText(result statusQueueEvidence) string {
	var text strings.Builder
	// strings.Builder writes cannot fail.
	_ = writeStatusQueueEvidence(&text, result)
	return text.String()
}

type statusQueueEvidence struct {
	Workflows        []readservice.QueueEligibilityView `json:"workflows"`
	OmittedWorkflows int                                `json:"omittedWorkflows"`
	Problem          string                             `json:"problem,omitempty"`
}

// Every redraw reopens the existing projection so a daemon rebuild/atomic
// replacement is visible. No provider reads, migrations, or history rebuilds.
func loadStatusQueueEvidence(ctx context.Context, sources readservice.LocalSources, workflows []apiv1.Workflow, gaggle, workflow string) statusQueueEvidence {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	store, err := readmodel.OpenExistingReader(ctx, sources.Layout.ReadDB())
	if err != nil {
		return statusQueueEvidence{Workflows: []readservice.QueueEligibilityView{}, Problem: "Queue projection unavailable; start the matching daemon and let its projection become ready."}
	}
	defer func() { _ = store.Close() }()
	sources.ReadModel = store
	reader, err := readservice.NewLocal(sources, func() bool { return true })
	if err != nil {
		return statusQueueEvidence{Workflows: []readservice.QueueEligibilityView{}, Problem: "Queue read service unavailable."}
	}
	return collectStatusQueueEvidence(ctx, reader, workflows, gaggle, workflow)
}

func collectStatusQueueEvidence(ctx context.Context, reader queueEvidenceReader, workflows []apiv1.Workflow, gaggle, workflow string) statusQueueEvidence {
	scopes := make([]statusWorkflowKey, 0, len(workflows))
	seen := make(map[statusWorkflowKey]bool)
	for _, def := range workflows {
		key := statusWorkflowKey{gaggle: def.Spec.Gaggle, workflow: def.Name}
		if (gaggle != "" && key.gaggle != gaggle) || (workflow != "" && key.workflow != workflow) || seen[key] {
			continue
		}
		seen[key] = true
		scopes = append(scopes, key)
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].gaggle == scopes[j].gaggle {
			return scopes[i].workflow < scopes[j].workflow
		}
		return scopes[i].gaggle < scopes[j].gaggle
	})
	result := statusQueueEvidence{Workflows: []readservice.QueueEligibilityView{}}
	if len(scopes) > maxStatusQueueWorkflows {
		result.OmittedWorkflows = len(scopes) - maxStatusQueueWorkflows
		scopes = scopes[:maxStatusQueueWorkflows]
	}
	for _, scope := range scopes {
		view, err := reader.QueueEligibility(ctx, scope.gaggle, scope.workflow)
		if err != nil {
			view = readservice.QueueEligibilityView{Gaggle: scope.gaggle, Workflow: scope.workflow, AsOf: time.Now().UTC(), Status: "unavailable", Problem: "Queue observation could not be read within this status request."}
		}
		result.Workflows = append(result.Workflows, view)
	}
	return result
}

func writeStatusQueueEvidence(w io.Writer, result statusQueueEvidence) error {
	if result.Problem != "" {
		_, err := fmt.Fprintf(w, "PR queue evidence unavailable: %s\n", result.Problem)
		return err
	}
	for _, view := range result.Workflows {
		if err := writeQueueEvidence(w, view, 0); err != nil {
			return err
		}
	}
	if result.OmittedWorkflows > 0 {
		_, err := fmt.Fprintf(w, "PR queue evidence: %d workflows omitted by the status bound; narrow --gaggle/--workflow or use queue-explain.\n", result.OmittedWorkflows)
		return err
	}
	return nil
}
