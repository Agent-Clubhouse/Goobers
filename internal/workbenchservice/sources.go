package workbenchservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/workbench"
)

// Sources reads only the currently authorized gaggle's explicit configuration.
// Provider credentials, connection references and human grants stay private.
func (s *Service) Sources(ctx context.Context, p httpapi.Principal, gaggle string) (workbench.SourcePage, error) {
	result := workbench.SourcePage{Items: []workbench.SourceView{}}
	if s == nil || s.Permissions == nil {
		return result, readError(http.StatusServiceUnavailable, "workbench_unavailable", "Backlog browsing is unavailable.")
	}
	err := s.Permissions.WithSourceView(ctx, p, gaggle, func(_ context.Context, g *apiv1.Gaggle) error {
		set, err := workbench.BindSources(*g)
		if err != nil {
			return readError(http.StatusConflict, "workbench_invalid_sources", "Workbench sources are not valid in the applied configuration.")
		}
		result.Generation = configDigest(g)
		for _, source := range set.Sources {
			result.Items = append(result.Items, sourceView(source))
		}
		return nil
	})
	return result, publicReadError(err)
}
func configDigest(g *apiv1.Gaggle) string {
	raw, _ := json.Marshal(g.Spec)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func sourceView(source workbench.BoundSource) workbench.SourceView {
	result := workbench.SourceView{BindingID: source.Spec.Name, Kind: source.Spec.Kind, Paths: append([]string(nil), source.Spec.Paths...)}
	if source.Spec.Kind == "backlog" {
		target := source.BacklogIdentity
		result.Provider = string(target.Provider)
		result.Owner = target.Owner
		result.Project = target.Project
		result.Repository = target.Name
	} else {
		target := source.Repository
		result.Provider = string(target.Provider)
		result.Owner = target.Owner
		result.Project = target.Project
		result.Repository = target.Name
		result.Branch = target.Branch
	}
	if source.Spec.Writes != nil {
		for _, field := range source.Spec.Writes.Fields {
			result.WriteFields = append(result.WriteFields, string(field))
		}
		for _, kind := range source.Spec.Writes.Relationships {
			result.WriteRelationships = append(result.WriteRelationships, string(kind))
		}
	}
	return result
}
