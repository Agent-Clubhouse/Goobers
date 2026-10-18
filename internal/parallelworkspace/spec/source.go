// Package spec holds the host-only boundary between runner orchestration and
// repository snapshot capture. No workflow or worker may supply these values.
package spec

import (
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// Source names immutable host journal artifacts and their exact fork commit.
type Source struct {
	SnapshotSHA string      `json:"snapshotSha"`
	Metadata    journal.Ref `json:"metadata"`
	Bundle      journal.Ref `json:"bundle"`
}

// Request binds capture or replay to one root parallel visit.
type Request struct {
	RunID, Gaggle, Parallel string
	Sequence                uint64
	At                      time.Time
	Repository              apiv1.RepoRef
	Workspace               string
}
