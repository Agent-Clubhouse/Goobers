// Package publicationpaths defines workspace-root paths owned by the harness,
// shared by checkout exclusion and publication admission.
package publicationpaths

import (
	"strings"

	"github.com/goobers/goobers/internal/gitexclude"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/mutationsidecar"
)

// launcherSessionPrefix names temporary native transcripts created at the
// workspace root by the Copilot wrapper-managed session adapter.
const launcherSessionPrefix = ".goobers-launcher-session-"

// IsReserved reports whether a canonical, slash-separated repository-relative
// path belongs to harness bookkeeping. Same-named paths nested inside the
// project remain legitimate source paths.
func IsReserved(path string) bool {
	root, _, _ := strings.Cut(path, "/")
	return root == ".goobers" || root == gooberassets.WorkspaceDir || root == mutationsidecar.FileName || strings.HasPrefix(root, launcherSessionPrefix)
}

// ExcludePatterns returns root-anchored exclusions for reserved paths. Omitting
// a trailing slash also excludes a file or symlink at a reserved directory name.
// Git exclusions leave tracked paths visible, including cleanup deletions.
func ExcludePatterns() []gitexclude.Pattern {
	return []gitexclude.Pattern{
		{Line: "/.goobers"},
		{Line: "/" + gooberassets.WorkspaceDir},
		{Line: "/" + mutationsidecar.FileName},
		{Line: "/" + launcherSessionPrefix + "*"},
	}
}
