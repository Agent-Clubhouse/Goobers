// Package recovery describes retained implementation state independently of
// the lifetime of a run's worktree or its provider-visible branch.
package recovery

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goobers/goobers/providers"
)

var (
	runIdentity = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,255}$`)
	gitObjectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	patchDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Record binds a retained ref to its original base, exact patch bytes, owner,
// and recovery deadline. It is not proof that the referenced objects exist;
// capture must verify durability before publishing it or authorizing cleanup.
type Record struct {
	Version       int    `json:"version"`
	RunID         string `json:"runId"`
	RepositoryKey string `json:"repositoryKey"`
	Ref           string `json:"ref"`
	// BaseRef is the remote ref selected by the owning run. It is optional so
	// version-1 records written before the field was introduced remain valid.
	BaseRef       string    `json:"baseRef,omitempty"`
	BaseSHA       string    `json:"baseSha"`
	SnapshotSHA   string    `json:"snapshotSha"`
	PatchDigest   string    `json:"patchDigest"`
	ArchiveDigest string    `json:"archiveDigest"`
	ArchiveBytes  int64     `json:"archiveBytes"`
	CreatedAt     time.Time `json:"createdAt"`
	RetainUntil   time.Time `json:"retainUntil"`
}

// RefForRun returns a private Git ref, never a provider branch name. Rejecting
// arbitrary ref syntax prevents a restore or reap request from naming main.
func RefForRun(runID string) (string, error) {
	if !runIdentity.MatchString(runID) {
		return "", fmt.Errorf("invalid recovery run identity")
	}
	return "refs/goobers/recovery/" + runID, nil
}

// RefForSnapshot gives each prepared snapshot its own immutable retention ref.
// A separate namespace allows older run-only refs to coexist without Git's
// file/directory ref-name collision. The owner remains the actual run ID.
func RefForSnapshot(runID, snapshotSHA string) (string, error) {
	if !runIdentity.MatchString(runID) || !gitObjectID.MatchString(snapshotSHA) {
		return "", fmt.Errorf("invalid recovery snapshot identity")
	}
	return "refs/goobers/recovery-snapshots/" + runID + "/" + snapshotSHA, nil
}

// Validate verifies record shape, not artifact existence or merge state.
func (r Record) Validate() error {
	if err := r.validateSnapshot(); err != nil {
		return err
	}
	if !patchDigest.MatchString(r.ArchiveDigest) || r.ArchiveBytes <= 0 {
		return fmt.Errorf("recovery record requires an archive digest and positive size")
	}
	return nil
}

// Snapshot preparation precedes archive creation. Only pin/bundle preparation
// may use this partial validation; published records must pass Validate.
func (r Record) validateSnapshot() error {
	if r.Version != 1 {
		return fmt.Errorf("unsupported recovery record version")
	}
	ref, err := RefForRun(r.RunID)
	if err != nil {
		return err
	}
	snapshotRef, snapshotErr := RefForSnapshot(r.RunID, r.SnapshotSHA)
	if r.Ref != ref && (snapshotErr != nil || r.Ref != snapshotRef) {
		return fmt.Errorf("recovery ref does not match run identity")
	}
	if !validRepositoryKey(r.RepositoryKey) {
		return fmt.Errorf("invalid recovery repository identity")
	}
	if r.BaseRef != "" && !validRecoveryBaseRef(r.BaseRef) {
		return fmt.Errorf("invalid recovery base ref")
	}
	if !gitObjectID.MatchString(r.BaseSHA) || !gitObjectID.MatchString(r.SnapshotSHA) || len(r.BaseSHA) != len(r.SnapshotSHA) {
		return fmt.Errorf("invalid recovery Git object identity")
	}
	if !patchDigest.MatchString(r.PatchDigest) {
		return fmt.Errorf("invalid recovery patch digest")
	}
	if r.CreatedAt.IsZero() || r.RetainUntil.IsZero() || !r.RetainUntil.After(r.CreatedAt) {
		return fmt.Errorf("recovery deadline must follow capture time")
	}
	return nil
}

func validRecoveryBaseRef(ref string) bool {
	if gitObjectID.MatchString(ref) {
		return true
	}
	if len(ref) > 4096 || !utf8.ValidString(ref) ||
		(!strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/")) ||
		strings.ContainsAny(ref, " ~^:?*[\\") || strings.Contains(ref, "..") ||
		strings.Contains(ref, "@{") || strings.Contains(ref, "//") ||
		strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") {
		return false
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func validRepositoryKey(key string) bool {
	if len(key) > 4096 || !utf8.ValidString(key) || strings.ContainsAny(key, "\x00\r\n") {
		return false
	}
	parts := strings.Split(key, "|")
	if len(parts) != 6 || parts[3] == "" || parts[4] == "" {
		return false
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderKind(parts[0]), URL: parts[1], Project: parts[2], Owner: parts[3], Name: parts[4], ID: parts[5]}
	if repo.CanonicalKey() != key {
		return false
	}
	switch repo.Provider {
	case providers.ProviderGitHub:
		return true
	case providers.ProviderADO:
		return repo.Project != ""
	case providers.ProviderGitea:
		return repo.URL != ""
	default:
		return false
	}
}
