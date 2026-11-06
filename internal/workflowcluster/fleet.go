package workflowcluster

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// FleetSnapshotVersion identifies the FleetSnapshot wire format (#8345).
const FleetSnapshotVersion = "workflowcluster.fleetsnapshot/v1"

// fleetHashLength is the hex length of opaque workflow keys and shape IDs.
const fleetHashLength = 16

// FleetSnapshot is one instance's workflow feature vectors, shaped for pooling
// across a fleet. It carries no workflow, gaggle, goober, stage, or goal text:
// workflows are opaque salted keys and goober names are salted hashes, so the
// same salt (the fleet ID) keeps shapes comparable across instances.
type FleetSnapshot struct {
	Version        string          `json:"version"`
	FeatureVersion string          `json:"featureVersion"`
	InstanceID     string          `json:"instanceId"`
	GoobersVersion string          `json:"goobersVersion,omitempty"`
	GeneratedAt    time.Time       `json:"generatedAt"`
	Workflows      []FleetWorkflow `json:"workflows"`
}

// FleetWorkflow is one workflow's features. Shape is a digest of the features,
// so identical workflows cloned across instances share a Shape.
type FleetWorkflow struct {
	Key         string              `json:"key"`
	Shape       string              `json:"shape"`
	Numeric     map[string]float64  `json:"numeric,omitempty"`
	Categorical map[string][]string `json:"categorical,omitempty"`
}

// fleetHashedSets are categorical features whose members are user-chosen names
// and are therefore salted-hashed before leaving the instance.
var fleetHashedSets = map[string]bool{"goobers": true}

// NewFleetSnapshot converts an instance's points into a FleetSnapshot. salt
// should be the fleet ID so every instance in a fleet hashes names alike.
func NewFleetSnapshot(featureVersion, instanceID, goobersVersion, salt string, points []Point, now time.Time) (FleetSnapshot, error) {
	if featureVersion == "" || instanceID == "" {
		return FleetSnapshot{}, errors.New("workflowcluster: fleet snapshot needs a feature version and instance ID")
	}
	sorted, err := validatePoints(points)
	if err != nil {
		return FleetSnapshot{}, err
	}
	snapshot := FleetSnapshot{
		Version: FleetSnapshotVersion, FeatureVersion: featureVersion, InstanceID: instanceID,
		GoobersVersion: goobersVersion, GeneratedAt: now.UTC(), Workflows: make([]FleetWorkflow, 0, len(sorted)),
	}
	for _, p := range sorted {
		categorical := make(map[string][]string, len(p.Categorical))
		for name, values := range p.Categorical {
			if !fleetHashedSets[name] {
				categorical[name] = append([]string(nil), values...)
				continue
			}
			hashed := make([]string, len(values))
			for i, v := range values {
				hashed[i] = fleetHash(salt, name, v)
			}
			categorical[name] = sortedUnique(hashed)
		}
		w := FleetWorkflow{Key: fleetHash(salt, instanceID, p.ID), Numeric: p.Numeric, Categorical: categorical}
		if w.Shape, err = fleetShape(w); err != nil {
			return FleetSnapshot{}, err
		}
		snapshot.Workflows = append(snapshot.Workflows, w)
	}
	sort.Slice(snapshot.Workflows, func(i, j int) bool { return snapshot.Workflows[i].Key < snapshot.Workflows[j].Key })
	return snapshot, nil
}

func fleetHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:fleetHashLength]
}

func fleetShape(w FleetWorkflow) (string, error) {
	numeric, categorical := w.Numeric, w.Categorical
	if numeric == nil {
		numeric = map[string]float64{}
	}
	if categorical == nil {
		categorical = map[string][]string{}
	}
	data, err := json.Marshal(struct {
		Numeric     map[string]float64  `json:"n"`
		Categorical map[string][]string `json:"c"`
	}{numeric, categorical})
	if err != nil {
		return "", fmt.Errorf("workflowcluster: shape digest: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:fleetHashLength], nil
}

// Fleet view statuses.
const (
	FleetClustered        = "clustered"
	FleetInsufficient     = "insufficient-data"
	fleetReasonSuperseded = "superseded by a newer snapshot from the same instance"
)

// FleetInstance reports how one instance's workflows sit in the fleet model.
// Aligned workflows fall in clusters shared with other instances, Private ones
// in clusters only this instance contributes to, and Outliers are noise.
type FleetInstance struct {
	InstanceID     string `json:"instanceId"`
	GoobersVersion string `json:"goobersVersion,omitempty"`
	Workflows      int    `json:"workflows"`
	Shapes         int    `json:"shapes"`
	Aligned        int    `json:"aligned"`
	Private        int    `json:"private"`
	Outliers       int    `json:"outliers"`
}

// FleetExclusion is a snapshot left out of the fleet model and why.
type FleetExclusion struct {
	InstanceID string `json:"instanceId"`
	Reason     string `json:"reason"`
}

// FleetShape is one distinct workflow shape and where it is deployed.
type FleetShape struct {
	Shape     string   `json:"shape"`
	Cluster   string   `json:"cluster,omitempty"`
	Workflows int      `json:"workflows"`
	Instances []string `json:"instances"`
}

// FleetCluster summarizes one cluster across the fleet. Shapes counts distinct
// shapes (what was clustered); Workflows and Instances count deployments.
type FleetCluster struct {
	ID             string        `json:"id"`
	MedoidShape    string        `json:"medoidShape"`
	Shapes         int           `json:"shapes"`
	Workflows      int           `json:"workflows"`
	Instances      int           `json:"instances"`
	Stability      float64       `json:"stability"`
	Stable         bool          `json:"stable"`
	Distinguishing []Attribution `json:"distinguishing,omitempty"`
}

// FleetView is the convergence of workflows across a fleet's instances.
type FleetView struct {
	Status         string           `json:"status"`
	FeatureVersion string           `json:"featureVersion,omitempty"`
	Workflows      int              `json:"workflows"`
	Population     int              `json:"population"`
	MinClusterSize int              `json:"minClusterSize"`
	Instances      []FleetInstance  `json:"instances"`
	Excluded       []FleetExclusion `json:"excluded"`
	Shapes         []FleetShape     `json:"shapes"`
	Clusters       []FleetCluster   `json:"clusters"`
	Model          *Model           `json:"model,omitempty"`
	// Confounds tests whether clusters merely track instances, labeling each
	// shape with its only instance, or "shared" when several deploy it.
	Confounds *ConfoundReport `json:"confounds,omitempty"`
	Map       []MapPoint      `json:"map,omitempty"`
}

type fleetShapeAcc struct {
	workflow  FleetWorkflow
	workflows int
	perInst   map[string]int
}

// ClusterFleet pools instance snapshots and clusters distinct shapes. A shape
// cloned onto many instances is one point, so a shared config repo cannot
// masquerade as convergence. Only the most common feature version is
// clustered; other snapshots are reported in Excluded.
func ClusterFleet(snapshots []FleetSnapshot, opts Options) (FleetView, error) {
	opts = opts.withDefaults()
	view := FleetView{MinClusterSize: opts.MinClusterSize, Instances: []FleetInstance{}, Excluded: []FleetExclusion{}, Shapes: []FleetShape{}, Clusters: []FleetCluster{}}

	latest := map[string]FleetSnapshot{}
	for _, s := range snapshots {
		if s.Version != FleetSnapshotVersion {
			return FleetView{}, fmt.Errorf("workflowcluster: snapshot from %q has unsupported version %q", s.InstanceID, s.Version)
		}
		if s.InstanceID == "" || s.FeatureVersion == "" {
			return FleetView{}, errors.New("workflowcluster: snapshot is missing an instance ID or feature version")
		}
		prev, ok := latest[s.InstanceID]
		if !ok {
			latest[s.InstanceID] = s
			continue
		}
		if s.GeneratedAt.After(prev.GeneratedAt) {
			latest[s.InstanceID] = s
		}
		view.Excluded = append(view.Excluded, FleetExclusion{InstanceID: s.InstanceID, Reason: fleetReasonSuperseded})
	}

	versions := map[string]int{}
	for _, s := range latest {
		versions[s.FeatureVersion]++
	}
	for v, n := range versions {
		if n > versions[view.FeatureVersion] || (n == versions[view.FeatureVersion] && v > view.FeatureVersion) {
			view.FeatureVersion = v
		}
	}

	ids := make([]string, 0, len(latest))
	for id := range latest {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	shapes := map[string]*fleetShapeAcc{}
	included := []FleetSnapshot{}
	for _, id := range ids {
		s := latest[id]
		if s.FeatureVersion != view.FeatureVersion {
			view.Excluded = append(view.Excluded, FleetExclusion{InstanceID: id,
				Reason: fmt.Sprintf("feature version %s; fleet clustered %s", s.FeatureVersion, view.FeatureVersion)})
			continue
		}
		included = append(included, s)
		for _, w := range s.Workflows {
			shape, err := fleetShape(w)
			if err != nil {
				return FleetView{}, err
			}
			if shape != w.Shape {
				return FleetView{}, fmt.Errorf("workflowcluster: snapshot from %q has a workflow whose shape does not match its features", id)
			}
			acc := shapes[shape]
			if acc == nil {
				acc = &fleetShapeAcc{workflow: w, perInst: map[string]int{}}
				shapes[shape] = acc
			}
			acc.workflows++
			acc.perInst[id]++
			view.Workflows++
		}
	}
	sort.Slice(view.Excluded, func(i, j int) bool { return view.Excluded[i].InstanceID < view.Excluded[j].InstanceID })

	points := make([]Point, 0, len(shapes))
	for shape, acc := range shapes {
		points = append(points, Point{ID: shape, Numeric: acc.workflow.Numeric, Categorical: acc.workflow.Categorical})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].ID < points[j].ID })
	view.Population = len(points)

	clusterOf := map[string]string{}
	view.Status = FleetInsufficient
	if len(points) >= opts.MinClusterSize {
		model, err := Fit(view.FeatureVersion, points, opts)
		if err != nil {
			return FleetView{}, err
		}
		view.Status = FleetClustered
		view.Model = &model
		view.Map = Project(model, points)
		for _, c := range model.Clusters {
			fc := FleetCluster{ID: c.ID, MedoidShape: c.Medoid.ID, Shapes: len(c.Members),
				Stability: c.Stability, Stable: c.Stable, Distinguishing: c.Distinguishing}
			instances := map[string]bool{}
			for _, m := range c.Members {
				clusterOf[m] = c.ID
				fc.Workflows += shapes[m].workflows
				for inst := range shapes[m].perInst {
					instances[inst] = true
				}
			}
			fc.Instances = len(instances)
			view.Clusters = append(view.Clusters, fc)
		}
		if len(included) > 1 {
			report := ConfoundCheck(model, shapeOwners(shapes), ConfoundOptions{Seed: opts.Seed})
			view.Confounds = &report
		}
	}

	clusterInstances := map[string]int{}
	for _, c := range view.Clusters {
		clusterInstances[c.ID] = c.Instances
	}
	for _, p := range points {
		acc := shapes[p.ID]
		insts := make([]string, 0, len(acc.perInst))
		for inst := range acc.perInst {
			insts = append(insts, inst)
		}
		sort.Strings(insts)
		view.Shapes = append(view.Shapes, FleetShape{Shape: p.ID, Cluster: clusterOf[p.ID], Workflows: acc.workflows, Instances: insts})
	}
	for _, s := range included {
		fi := FleetInstance{InstanceID: s.InstanceID, GoobersVersion: s.GoobersVersion, Workflows: len(s.Workflows)}
		distinct := map[string]bool{}
		for _, w := range s.Workflows {
			distinct[w.Shape] = true
			cluster, ok := clusterOf[w.Shape]
			switch {
			case !ok:
				fi.Outliers++
			case clusterInstances[cluster] > 1:
				fi.Aligned++
			default:
				fi.Private++
			}
		}
		fi.Shapes = len(distinct)
		if view.Status != FleetClustered {
			fi.Outliers = 0
		}
		view.Instances = append(view.Instances, fi)
	}
	return view, nil
}

// sharedStratum labels shapes deployed on more than one instance.
const sharedStratum = "shared"

// shapeOwners labels each shape with the one instance deploying it, or
// "shared", so the confound check asks whether clusters are just one
// instance's private workflows.
func shapeOwners(shapes map[string]*fleetShapeAcc) Strata {
	strata := make(Strata, len(shapes))
	for shape, acc := range shapes {
		strata[shape] = sharedStratum
		if len(acc.perInst) == 1 {
			for inst := range acc.perInst {
				strata[shape] = inst
			}
		}
	}
	return strata
}
