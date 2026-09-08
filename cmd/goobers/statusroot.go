package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

func validateStartupRoot(layout instance.Layout) error {
	if _, err := os.Stat(layout.ConfigFile()); err != nil {
		return fmt.Errorf("%s not found (not an instance root — run `goobers init` first)", layout.ConfigFile())
	}
	return instance.RequireCurrentRoot(layout.Root)
}

type statusRootIdentity struct {
	DecommissionedAt   *time.Time `json:"decommissionedAt,omitempty"`
	DecommissionReason string     `json:"decommissionReason,omitempty"`
	LifecycleProblem   string     `json:"lifecycleProblem,omitempty"`
	Path               string     `json:"path"`
	ID                 string     `json:"id,omitempty"`
	IdentityProblem    string     `json:"identityProblem,omitempty"`
	DaemonState        string     `json:"daemonState"`
	OwningPID          int        `json:"owningPid,omitempty"`
	RecordedPID        int        `json:"recordedPid,omitempty"`
	RecordedRoot       string     `json:"recordedRoot,omitempty"`
	DaemonProblem      string     `json:"daemonProblem,omitempty"`
}

func canonicalStatusRoot(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	return absolute
}

func inspectStatusRoot(layout instance.Layout, now time.Time) statusRootIdentity {
	result := statusRootIdentity{Path: canonicalStatusRoot(layout.Root), DaemonState: "not-running"}
	readStatusRootLifecycle(&result, layout.Root)
	id, err := instance.ReadRootIdentity(layout.Root)
	switch {
	case err == nil:
		result.ID = id
	case errors.Is(err, os.ErrNotExist):
		result.IdentityProblem = "Legacy root has no durable identity; inspection does not adopt it."
	default:
		result.IdentityProblem = "Root identity is invalid or unreadable; do not infer identity from database freshness."
	}
	running, owner, liveness, err := inspectDaemonLiveness(filepath.Join(layout.SchedulerDir(), "up.lock"), now)
	if owner != nil {
		result.RecordedPID, result.RecordedRoot = owner.PID, owner.InstanceRoot
	}
	if err != nil {
		result.DaemonState, result.DaemonProblem = "unknown", "Daemon ownership or heartbeat could not be verified."
		return result
	}
	if !running {
		return result
	}
	if owner == nil || owner.PID <= 0 || owner.InstanceRoot == "" || canonicalStatusRoot(owner.InstanceRoot) != result.Path {
		result.DaemonState, result.DaemonProblem = "ownership-unverified", "The held daemon lock does not identify this root and a valid owning PID."
		return result
	}
	result.OwningPID = owner.PID
	result.DaemonState = "unhealthy"
	if liveness.Healthy {
		result.DaemonState = "healthy"
	}
	return result
}

func optionalStatusRoot(enabled bool, layout instance.Layout, now time.Time) *statusRootIdentity {
	if !enabled {
		return nil
	}
	result := inspectStatusRoot(layout, now)
	return &result
}

func writeStatusRoot(w io.Writer, root statusRootIdentity) {
	pf(w, "Instance root: %q; instance ID: %q; daemon: %s; owning PID: %d\n", root.Path, root.ID, root.DaemonState, root.OwningPID)
	if root.DecommissionedAt != nil {
		pf(w, "  Historical root; do not use. Decommissioned %s: %q\n", root.DecommissionedAt.Format(time.RFC3339Nano), root.DecommissionReason)
	}
	if root.RecordedPID != 0 {
		pf(w, "  Recorded daemon PID: %d; recorded root: %q (recorded metadata alone is not liveness)\n", root.RecordedPID, root.RecordedRoot)
	}
	for _, problem := range []string{root.IdentityProblem, root.DaemonProblem, root.LifecycleProblem} {
		if problem != "" {
			pf(w, "  Root warning: %s\n", problem)
		}
	}
}

func readStatusRootLifecycle(result *statusRootIdentity, root string) {
	marker, err := instance.ReadRootDecommission(root)
	if err == nil {
		result.DecommissionedAt, result.DecommissionReason = &marker.At, marker.Reason
	} else if !errors.Is(err, os.ErrNotExist) {
		result.LifecycleProblem = "Historical-root marker is invalid or unreadable; root authority is unknown."
	}
}

func statusRootText(layout instance.Layout, now time.Time) string {
	var text strings.Builder
	writeStatusRoot(&text, inspectStatusRoot(layout, now))
	return text.String()
}
