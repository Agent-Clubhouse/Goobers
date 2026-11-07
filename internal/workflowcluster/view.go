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
	// ConfoundsByAxis contains every supported structural provenance check,
	// including the task-cohort axis.
	ConfoundsByAxis map[string]ConfoundReport `json:"confoundsByAxis,omitempty"`
	// Cohorts lists the task-cohort labels present in Map.
	Cohorts []string `json:"cohorts,omitempty"`
	// Map places every workflow in two dimensions for the Portal's scatter map.
	Map []MapPoint `json:"map,omitempty"`
	// Trends compares retained agent-authored child shapes against Model.
	Trends *TrendReport `json:"trends,omitempty"`
	// TrendsUnavailable explains why a clustered view carries no Trends.
	TrendsUnavailable string `json:"trendsUnavailable,omitempty"`
	// TrendsErr is the trend scan failure behind TrendsUnavailable, for
	// server-side logging only.
	TrendsErr error `json:"-"`
	// Fitness summarizes retained terminal runs for each structural cluster.
	Fitness *FitnessReport `json:"fitness,omitempty"`
	// FitnessUnavailable explains why a clustered view carries no Fitness.
	FitnessUnavailable string `json:"fitnessUnavailable,omitempty"`
	// FitnessErr is the scan failure behind FitnessUnavailable.
	FitnessErr error `json:"-"`
	// Templates identifies shipped templates, configured workflow provenance,
	// and clusters converging beyond those templates.
	Templates *TemplateReport `json:"templates,omitempty"`
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
	points, strata, digests, err := configuredPoints(s.Root)
	if err != nil {
		return View{}, err
	}
	view := View{Status: ViewInsufficientData, Population: len(points), MinClusterSize: cliMinClusterSize}
	if len(points) < cliMinClusterSize {
		templates, _, err := analyzeTemplates(points, strata[StratifyTemplate], nil)
		if err != nil {
			return View{}, err
		}
		view.Templates = &templates
		return view, nil
	}
	model, err := Fit(StructuralFeatureVersion, points, Options{MinClusterSize: cliMinClusterSize})
	if err != nil {
		return View{}, err
	}
	confoundsByAxis := map[string]ConfoundReport{}
	templates, templateStrata, err := analyzeTemplates(points, strata[StratifyTemplate], &model)
	if err != nil {
		return View{}, err
	}
	strata[StratifyTemplate] = templateStrata
	for _, axis := range append([]string{StratifyGaggle, StratifyCohort, StratifyTemplate}, provenanceAxes...) {
		confoundsByAxis[axis] = ConfoundCheck(model, strata[axis], ConfoundOptions{})
	}
	confounds := confoundsByAxis[StratifyGaggle]
	view.Status = ViewClustered
	view.Model = &model
	view.Confounds = &confounds
	view.ConfoundsByAxis = confoundsByAxis
	view.Cohorts = cohortLabels(points)
	view.Map = Project(model, points)
	for i := range view.Map {
		provenance := templates.Workflows[view.Map[i].ID]
		view.Map[i].TemplateProvenance = &provenance
	}
	view.Templates = &templates
	scan, fitnessErr := ScanBehavior(s.Root, BehaviorScanOptions{})
	if fitnessErr == nil {
		fitness := EvaluateFitness(model, digests, scan, DefaultFitnessMinRuns)
		view.Fitness = &fitness
	} else {
		view.FitnessUnavailable = "cluster fitness could not be computed from retained journals"
		view.FitnessErr = fitnessErr
	}
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
