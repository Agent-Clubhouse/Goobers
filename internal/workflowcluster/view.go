package workflowcluster

import "context"

// View statuses.
const (
	// ViewClustered means Model and Confounds are present.
	ViewClustered = "clustered"
	// ViewInsufficientData means the instance configures fewer workflows than
	// the minimum cluster size, so nothing was fitted.
	ViewInsufficientData = "insufficient-data"
)

// View is the Labs read contract for the configured instance's workflow
// clusters. Model and Confounds are exactly what
// `goobers workflow clusters --json --stratify gaggle` prints. The view is
// advisory: it never changes configuration or gates.
type View struct {
	Status         string          `json:"status"`
	Population     int             `json:"population"`
	MinClusterSize int             `json:"minClusterSize"`
	Model          *Model          `json:"model,omitempty"`
	Confounds      *ConfoundReport `json:"confounds,omitempty"`
}

// ConfiguredSource serves the cluster View of the instance rooted at Root.
type ConfiguredSource struct {
	Root string
}

// WorkflowClusters loads the configured workflows, fits them with the CLI
// defaults, and tests the clusters for a gaggle confound.
func (s ConfiguredSource) WorkflowClusters(ctx context.Context) (View, error) {
	if err := ctx.Err(); err != nil {
		return View{}, err
	}
	points, err := ConfiguredPoints(s.Root)
	if err != nil {
		return View{}, err
	}
	view := View{Status: ViewInsufficientData, Population: len(points), MinClusterSize: cliMinClusterSize}
	if len(points) < cliMinClusterSize {
		return view, nil
	}
	model, err := Fit(StructuralFeatureVersion, points, Options{MinClusterSize: cliMinClusterSize})
	if err != nil {
		return View{}, err
	}
	confounds := ConfoundCheck(model, gaggleStrata(points), ConfoundOptions{})
	view.Status = ViewClustered
	view.Model = &model
	view.Confounds = &confounds
	return view, nil
}
