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
	// Map places every workflow in two dimensions for the Portal's scatter map.
	Map []MapPoint `json:"map,omitempty"`
	// Trends compares retained agent-authored child shapes against Model.
	Trends *TrendReport `json:"trends,omitempty"`
	// TrendsUnavailable explains why a clustered view carries no Trends.
	TrendsUnavailable string `json:"trendsUnavailable,omitempty"`
	// TrendsErr is the trend scan failure behind TrendsUnavailable, for
	// server-side logging only.
	TrendsErr error `json:"-"`
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
	view.Map = Project(model, points)
	trends, err := ScanChildTrends(ctx, s.Root, model, TrendScanOptions{})
	switch {
	case err == nil:
		view.Trends = &trends
	case ctx.Err() != nil:
		return View{}, ctx.Err()
	default:
		// Trends are supplementary; the clustered view is still served.
		view.TrendsUnavailable = "child-workflow trends could not be computed from retained journals"
		view.TrendsErr = err
	}
	return view, nil
}
