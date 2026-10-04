package providers

import (
	"context"
	"crypto/sha1" // Git object identity, not a security signature.
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds for declared repository-source reads, before parsing or projection.
const (
	MaxRepositorySourceBytes         = 1 << 20
	MaxRepositorySourceResponseBytes = 2 << 20
	MaxRepositorySourceDepth         = 16
	MaxRepositorySourceTreeEntries   = 2000
)

// ErrRepositorySource refuses missing, nonregular, unpinned or unverifiable source
// data. It never means that a previously indexed source should be deleted.
var ErrRepositorySource = errors.New("provider: repository source is unavailable or unverifiable")

// RepositorySourceReader observes only caller-supplied trusted repository scope.
// Workbench adapters restrict it further to declared branch and literal paths.
type RepositorySourceReader interface {
	ReadSourceBranch(context.Context, RepositoryRef, string) (string, error)
	ReadRepositorySource(context.Context, RepositoryRef, string, string) (RepositorySourceFile, error)
}

// RepositorySourceFile is exact regular-file custody at one immutable commit.
// ETag is transport metadata only, not a substitute for commit/blob identity.
type RepositorySourceFile struct {
	Commit, Path, BlobID, ETag string
	Content                    []byte
}

// ValidRepositorySourcePath accepts bounded literal paths without traversal.
func ValidRepositorySourcePath(value string) bool {
	return value != "" && len(value) <= 1024 && utf8.ValidString(value) && path.Clean(value) == value && value != "." && value != ".." && !path.IsAbs(value) && !strings.HasPrefix(value, "../") && !strings.ContainsAny(value, "\\:?#*") && strings.IndexFunc(value, unicode.IsControl) < 0 && len(strings.Split(value, "/")) <= MaxRepositorySourceDepth
}

// ValidSourceCommit restricts repository provenance to canonical Git SHA-1 IDs
// supported by these provider endpoints. It is not an arbitrary branch/ref.
func ValidSourceCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

// ValidateRepositorySourceFile verifies exact path/commit and Git blob bytes.
func ValidateRepositorySourceFile(file RepositorySourceFile, path, commit string) error {
	if !ValidSourceCommit(commit) || file.Commit != commit || file.Path != path || !ValidSourceCommit(file.BlobID) || len(file.Content) > MaxRepositorySourceBytes {
		return ErrRepositorySource
	}
	if sourceBlobID(file.Content) != file.BlobID {
		return ErrRepositorySource
	}
	return nil
}

func sourceBlobID(content []byte) string {
	hash := sha1.New() //nolint:gosec // Required Git blob identity, verified independently from API metadata.
	_, _ = fmt.Fprintf(hash, "blob %d\x00", len(content))
	_, _ = hash.Write(content)
	return hex.EncodeToString(hash.Sum(nil))
}

func sourceBranchName(branch string) bool {
	return branch != "" && len(branch) <= 1024 && strings.TrimSpace(branch) == branch && !strings.ContainsAny(branch, "\x00\r\n\\?#") && !strings.Contains(branch, "..") && !strings.HasPrefix(branch, "/") && !strings.HasSuffix(branch, "/")
}

func sourceETag(value string) string {
	if len(value) > 512 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	return value
}
