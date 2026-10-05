package workbenchservice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchgraph"
	"github.com/goobers/goobers/internal/workbenchprovider"
)

const graphBacklogItems = 25

// Graph collects only server-selected pages under one applied authorization and
// configuration lease. It accepts no client graph, source pages, URL, or cursor.
// V1 gives each declared source at most one page in declaration order; later
// windows remain explicit partial coverage and require ordinary source browsing.
func (s *Service) Graph(ctx context.Context, p httpapi.Principal, gaggle string) (workbenchgraph.Graph, error) {
	if s == nil || s.Permissions == nil {
		return workbenchgraph.Graph{}, readError(http.StatusServiceUnavailable, "workbench_unavailable", "Workbench graph browsing is unavailable.")
	}
	var result workbenchgraph.Graph
	err := s.Permissions.WithSourceSnapshot(ctx, p, gaggle, func(ctx context.Context, g *apiv1.Gaggle, load interactiveaccess.SnapshotCredentialLoader) error {
		set, err := workbench.BindSources(*g)
		if err != nil {
			return readError(http.StatusConflict, "workbench_invalid_sources", "Workbench sources are not valid in the applied configuration.")
		}
		if len(set.Sources) == 0 {
			result = emptyGraph(g.Name)
		} else {
			result, err = s.collectGraph(ctx, set, configDigest(g), load)
			if err != nil {
				return err
			}
		}
		result.Generation = configDigest(g)
		return nil
	})
	if err != nil {
		return workbenchgraph.Graph{}, publicGraphError(err)
	}
	return result, nil
}

func (s *Service) collectGraph(ctx context.Context, set workbench.SourceSet, generation string, load interactiveaccess.SnapshotCredentialLoader) (workbenchgraph.Graph, error) {
	snapshot := workbenchgraph.Snapshot{Sources: set}
	reasons := map[string]string{}
	used := 0
	exhausted := false
	for _, source := range set.Sources {
		if err := ctx.Err(); err != nil {
			return workbenchgraph.Graph{}, err
		}
		bound := workbench.MaxDocumentPageBytes
		if source.Spec.Kind == "backlog" {
			bound = workbench.MaxBacklogPageBytes
		}
		if exhausted || bound > workbenchgraph.MaxSnapshotBytes-used {
			exhausted = true
			reasons[source.Spec.Name] = "aggregate-budget"
			continue
		}
		binding := ReadBinding{Scope: set.Scope, Source: source, Generation: generation}
		window, err := s.graphSource(ctx, binding, load)
		if err != nil {
			reasons[source.Spec.Name] = "source-not-loaded"
			continue
		}
		raw, err := json.Marshal(window.value())
		if err != nil || len(raw) > bound {
			return workbenchgraph.Graph{}, workbenchgraph.ErrProjectionBound
		}
		used += len(raw)
		window.appendTo(&snapshot)
	}
	if err := ctx.Err(); err != nil {
		return workbenchgraph.Graph{}, err
	}
	graph, err := workbenchgraph.Project(snapshot)
	if err != nil {
		return workbenchgraph.Graph{}, err
	}
	for index := range graph.Sources {
		if reason := reasons[graph.Sources[index].SourceBindingID]; reason != "" {
			graph.Sources[index].Reasons = append(graph.Sources[index].Reasons, reason)
			graph.Partial = true
		}
	}
	return graph, nil
}

type graphWindow struct {
	backlog   *workbenchgraph.BacklogWindow
	documents *workbench.DocumentPage
}

func (w graphWindow) value() any {
	if w.backlog != nil {
		return w.backlog.Page
	}
	return w.documents
}
func (w graphWindow) appendTo(snapshot *workbenchgraph.Snapshot) {
	if w.backlog != nil {
		snapshot.Backlogs = append(snapshot.Backlogs, *w.backlog)
	} else {
		snapshot.Documents = append(snapshot.Documents, *w.documents)
	}
}
func (s *Service) graphSource(ctx context.Context, binding ReadBinding, load interactiveaccess.SnapshotCredentialLoader) (graphWindow, error) {
	action := apiv1.InteractiveAction("repository.read")
	target := interactiveaccess.Target{Kind: "repository"}
	if binding.Source.Spec.Kind == "backlog" {
		action = "backlog.read"
		target.Kind = "backlog"
	} else {
		target.Repository = *binding.Source.Spec.Repository
	}
	// Authorization is checked even when the server lacks a configured adapter.
	credential, err := load(ctx, action, target)
	if err != nil {
		return graphWindow{}, err
	}
	if binding.Source.Spec.Kind == "backlog" {
		return s.graphBacklog(ctx, binding, credential)
	}
	return s.graphDocuments(ctx, binding, credential)
}
func (s *Service) graphBacklog(ctx context.Context, binding ReadBinding, credential interactiveaccess.Credential) (graphWindow, error) {
	if s.Backlog == nil {
		return graphWindow{}, workbenchprovider.ErrInvalidSource
	}
	var page workbench.BacklogPage
	err := s.useBacklog(ctx, binding, credential, func(ctx context.Context, reader *workbenchprovider.BacklogReader) error {
		var err error
		page, err = reader.Page(ctx, workbench.BacklogPageRequest{Limit: graphBacklogItems})
		return err
	})
	return graphWindow{backlog: &workbenchgraph.BacklogWindow{SourceBindingID: binding.Source.Spec.Name, Page: page}}, err
}
func (s *Service) graphDocuments(ctx context.Context, binding ReadBinding, credential interactiveaccess.Credential) (graphWindow, error) {
	if s.Repository == nil {
		return graphWindow{}, workbenchprovider.ErrInvalidSource
	}
	client, err := s.Repository(ctx, binding, credential)
	if err != nil {
		return graphWindow{}, err
	}
	reader, err := workbenchprovider.NewRepositoryReader(binding.Scope, binding.Source, client)
	if err != nil {
		return graphWindow{}, err
	}
	page, err := reader.Page(ctx, workbench.DocumentPageRequest{Limit: workbench.MaxDocumentPageFiles})
	return graphWindow{documents: &page}, err
}
func publicGraphError(err error) error {
	switch {
	case errors.Is(err, workbenchgraph.ErrInvalidSnapshot):
		return readError(http.StatusConflict, "workbench_graph_changed", "Source revisions changed during graph collection. Restart the read.")
	case errors.Is(err, workbenchgraph.ErrProjectionBound):
		return readError(http.StatusBadGateway, "workbench_graph_bound", "The source exceeded a bounded graph projection limit.")
	default:
		return publicReadError(err)
	}
}
func emptyGraph(gaggle string) workbenchgraph.Graph {
	return workbenchgraph.Graph{GaggleID: gaggle, Nodes: []workbenchgraph.Node{}, Edges: []workbenchgraph.Edge{}, Documents: []workbenchgraph.Document{}, Aliases: []workbenchgraph.Alias{}, Sources: []workbenchgraph.Coverage{}, Conflicts: []workbenchgraph.Conflict{}}
}
