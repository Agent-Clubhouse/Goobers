package providers

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Repair limits apply before provider decoding, mutation, and retained custody.
const (
	MaxPRRepairFiles         = 32
	MaxPRRepairContentBytes  = 1 << 20
	MaxPRRepairResponseBytes = 2 << 20
	MaxPRRepairDuration      = 8 * time.Second
)

// ErrPRRepair refuses stale, foreign, unbounded, or unverifiable repair custody.
var ErrPRRepair = errors.New("provider: pull request repair source changed or is unverifiable")

// RepairPullRequest is a same-repository PR observation. It conveys no authority:
// callers separately bind human selection, current permissions and writer custody.
type RepairPullRequest struct {
	Repository   RepositoryRef `json:"repository"`
	RepositoryID string        `json:"repositoryId"`
	ID           string        `json:"id"`
	StableID     string        `json:"stableId"`
	Title        string        `json:"title"`
	Body         string        `json:"body"`
	URL          string        `json:"url"`
	Head         string        `json:"head"`
	Base         string        `json:"base"`
	HeadSHA      string        `json:"headSha"`
	BaseSHA      string        `json:"baseSha"`
	Open         bool          `json:"open"`
	Draft        bool          `json:"draft"`
}

// PRRepairReader never follows a PR fork or an arbitrary provider-returned URL.
type PRRepairReader interface {
	InspectRepairPullRequest(context.Context, RepositoryRef, string) (RepairPullRequest, error)
	ReadRepairFile(context.Context, RepairPullRequest, string) (RepairFile, error)
}

// RepairFile distinguishes verified absence from unreadable or nonregular paths.
// Content is UTF-8 text only; native commit/blob identity remains authoritative.
type RepairFile struct {
	Commit  string `json:"commit"`
	Path    string `json:"path"`
	Present bool   `json:"present"`
	BlobID  string `json:"blobId,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Content string `json:"content,omitempty"`
}

// PRRepairChange supports regular UTF-8 mode-100644 additions/edits.
// Executable-file edits are refused before publication. Deletion uses nil Content;
// add uses empty PreviousBlob. Neither symlink/submodule nor rename is implicit.
type PRRepairChange struct {
	Path         string  `json:"path"`
	PreviousBlob string  `json:"previousBlob,omitempty"`
	Content      *string `json:"content,omitempty"`
}

// PullRequestRepair is trusted retained intent. CommandID and Message must come
// from the host command ledger, not an unscoped provider token or model identity.
type PullRequestRepair struct {
	Target    RepairPullRequest `json:"target"`
	CommandID string            `json:"commandId"`
	Message   string            `json:"message"`
	Changes   []PRRepairChange  `json:"changes"`
}

// PRRepairResult distinguishes sending an effect from its native acknowledgement.
type PRRepairResult struct {
	MutationAttempted bool
	Acknowledged      bool
	CommitID          string
}

// PRRepairObservation never implies the original request was acknowledged.
type PRRepairObservation struct {
	Matches  bool
	CommitID string
}

// PRRepairWriter performs one exact-head publication or read-only reconciliation.
type PRRepairWriter interface {
	ApplyPullRequestRepair(context.Context, PullRequestRepair) (PRRepairResult, error)
	ObservePullRequestRepair(context.Context, PullRequestRepair) (PRRepairObservation, error)
}

func repairContext(ctx context.Context) (context.Context, context.CancelFunc) {
	bounded, cancel := context.WithTimeout(ctx, MaxPRRepairDuration)
	return WithResponseBodyLimit(bounded, MaxPRRepairResponseBytes), cancel
}
func validRepairTarget(value RepairPullRequest) bool {
	return value.RepositoryID != "" && len(value.RepositoryID) <= 128 && nativePositiveID(value.ID) && value.StableID != "" && len(value.StableID) <= 128 && sourceBranchName(value.Head) && sourceBranchName(value.Base) && value.Head != value.Base && ValidSourceCommit(value.HeadSHA) && ValidSourceCommit(value.BaseSHA) && len(value.Title) <= 8192 && len(value.Body) <= 64<<10 && len(value.URL) <= 2048
}
func sameRepairTarget(expected, actual RepairPullRequest) bool {
	return expected.Repository == actual.Repository && expected.RepositoryID == actual.RepositoryID && expected.ID == actual.ID && expected.StableID == actual.StableID && expected.Head == actual.Head && expected.Base == actual.Base
}
func freshRepairTarget(ctx context.Context, reader PRRepairReader, target RepairPullRequest) error {
	if !validRepairTarget(target) || !target.Open {
		return ErrPRRepair
	}
	current, err := reader.InspectRepairPullRequest(ctx, target.Repository, target.ID)
	if err != nil {
		return err
	}
	if !sameRepairTarget(target, current) || !current.Open || current.HeadSHA != target.HeadSHA {
		return ErrPRRepair
	}
	return nil
}
func validRepairPath(name string) bool {
	if !ValidRepositorySourcePath(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		switch strings.ToLower(part) {
		case ".git", ".goobers", ".goober-assets":
			return false
		}
	}
	return true
}

func repairTreeEntry(entries []sourceTreeEntry, part string, leaf bool) (sourceTreeEntry, bool, error) {
	var found *sourceTreeEntry
	for _, entry := range entries {
		if entry.Path == part {
			if found != nil || !ValidSourceCommit(entry.SHA) {
				return sourceTreeEntry{}, false, ErrPRRepair
			}
			copy := entry
			found = &copy
		}
	}
	if found == nil {
		return sourceTreeEntry{}, false, nil
	}
	if leaf {
		if found.Type != "blob" || (found.Mode != "100644" && found.Mode != "100755") || found.Size < 0 || found.Size > MaxRepositorySourceBytes {
			return sourceTreeEntry{}, false, ErrPRRepair
		}
	} else if found.Type != "tree" || found.Mode != "040000" {
		return sourceTreeEntry{}, false, ErrPRRepair
	}
	return *found, true, nil
}
