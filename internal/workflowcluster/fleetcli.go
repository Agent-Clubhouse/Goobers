package workflowcluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/fleet"
	"github.com/goobers/goobers/internal/version"
)

// maxFleetSnapshotBytes bounds each snapshot file read by --fleet.
const maxFleetSnapshotBytes = 32 << 20

// snapshotIdentity resolves the instance ID and salt for --snapshot. Explicit
// flags win; otherwise a joined instance uses its fleet association, so every
// instance in one fleet hashes names with the same salt (the fleet ID).
func snapshotIdentity(root, instanceID, salt string, saltSet bool) (string, string, error) {
	if instanceID != "" && saltSet {
		return instanceID, salt, nil
	}
	var association fleet.Association
	storage, err := fleet.NewFileStorage("")
	if err == nil {
		association, err = storage.LoadAssociation(root)
	}
	switch {
	case err == nil:
	case errors.Is(err, fleet.ErrNotAssociated) && instanceID != "":
	case errors.Is(err, fleet.ErrNotAssociated):
		return "", "", errors.New("instance has not joined a fleet; pass --instance-id (and --salt shared by the fleet)")
	default:
		return "", "", fmt.Errorf("read fleet association: %w", err)
	}
	if instanceID == "" {
		instanceID = association.InstanceID
	}
	if !saltSet {
		salt = association.FleetID
	}
	return instanceID, salt, nil
}

func writeSnapshot(stdout io.Writer, snapshot FleetSnapshot) error {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(snapshot)
}

// readFleetSnapshots reads every *.json snapshot directly under dir.
func readFleetSnapshots(dir string) ([]FleetSnapshot, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no *.json snapshots in %s", dir)
	}
	sort.Strings(paths)
	snapshots := make([]FleetSnapshot, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if info.Size() > maxFleetSnapshotBytes {
			return nil, fmt.Errorf("%s: snapshot exceeds %d bytes", filepath.Base(path), maxFleetSnapshotBytes)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var s FleetSnapshot
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		snapshots = append(snapshots, s)
	}
	return snapshots, nil
}

func runFleetCLI(dir string, opts Options, asJSON bool, stdout, stderr io.Writer) int {
	snapshots, err := readFleetSnapshots(dir)
	if err == nil {
		var view FleetView
		if view, err = ClusterFleet(snapshots, opts); err == nil {
			if asJSON {
				encoder := json.NewEncoder(stdout)
				encoder.SetIndent("", "  ")
				err = encoder.Encode(view)
			} else {
				writeFleetReport(stdout, view)
			}
		}
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func writeFleetReport(w io.Writer, v FleetView) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	p("Labs: experimental, advisory workflow convergence across %d instances.\n", len(v.Instances))
	p("%d workflows, %d distinct shapes (%s)\n", v.Workflows, v.Population, v.FeatureVersion)
	if v.Status != FleetClustered {
		p("not enough distinct shapes to cluster (need %d)\n", v.MinClusterSize)
	} else {
		p("model %s (%d clusters, %d noise shapes)\n", v.Model.ID, len(v.Clusters), len(v.Model.Noise))
		for _, c := range v.Clusters {
			stable := "stable"
			if !c.Stable {
				stable = "UNSTABLE"
			}
			p("\n%s  %d shapes  %d workflows  on %d/%d instances  stability %.2f (%s)\n",
				c.ID, c.Shapes, c.Workflows, c.Instances, len(v.Instances), c.Stability, stable)
			p("  medoid shape  %s\n", c.MedoidShape)
			for _, a := range c.Distinguishing {
				name := a.Feature
				if a.Value != "" {
					name += "=" + a.Value
				}
				p("  %+.2f    %s\n", a.Effect, name)
			}
		}
	}
	p("\nInstances (aligned = shared with other instances, private = only here, outliers = noise)\n")
	for _, i := range v.Instances {
		p("  %s  %d workflows  %d shapes  aligned %d  private %d  outliers %d\n",
			i.InstanceID, i.Workflows, i.Shapes, i.Aligned, i.Private, i.Outliers)
	}
	if len(v.Excluded) > 0 {
		p("\nExcluded\n")
		for _, e := range v.Excluded {
			p("  %s  %s\n", e.InstanceID, e.Reason)
		}
	}
	if v.Confounds != nil {
		writeConfounds(w, "instance", *v.Confounds)
	}
}

func runSnapshotCLI(root, featureVersion, instanceID, salt string, saltSet bool, points []Point, stdout, stderr io.Writer) int {
	id, salt, err := snapshotIdentity(root, instanceID, salt, saltSet)
	if err == nil {
		var s FleetSnapshot
		if s, err = NewFleetSnapshot(featureVersion, id, version.Get().Version, salt, points, time.Now()); err == nil {
			err = writeSnapshot(stdout, s)
		}
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	return 0
}
