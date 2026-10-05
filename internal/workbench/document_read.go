package workbench

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

// Repository page bounds apply to literal configured files, never a crawl.
const (
	MaxDocumentPageFiles = 8
	MaxDocumentPageBytes = 4 << 20
)

// DocumentPageRequest accepts only an opaque continuation and bounded file count.
// It cannot select another path, repository, branch or historical commit.
type DocumentPageRequest struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// SourceProvenance identifies exact source bytes. ETag is transport metadata,
// never a persistent node ID or an optimistic Git revision guard.
type SourceProvenance struct {
	Commit        string `json:"commit"`
	BlobID        string `json:"blobId"`
	ContentDigest string `json:"contentDigest"`
	ETag          string `json:"etag,omitempty"`
}

// DocumentFileRead is one configured file observation. Status is available,
// unavailable, invalid-source or oversized. Only available files have content.
// Ref is present only for an explicitly identified objective. Ordinary Markdown
// remains reference material identified by its path and immutable provenance.
type DocumentFileRead struct {
	Path       string             `json:"path"`
	Status     string             `json:"status"`
	Provenance *SourceProvenance  `json:"provenance,omitempty"`
	Ref        *NodeRef           `json:"ref,omitempty"`
	Body       string             `json:"body,omitempty"`
	Objective  *ObjectiveMetadata `json:"objective,omitempty"`
	Manifest   *Manifest          `json:"manifest,omitempty"`
}

// DocumentPage is a bounded window over the explicitly declared source paths.
// Coverage is complete only when this one page covers every configured path
// successfully from offset zero. Multi-page callers must compose equal target
// and commit pins themselves; neither exhaustion nor absence authorizes deletion.
type DocumentPage struct {
	SourceBindingID    string                              `json:"sourceBindingId"`
	Repository         apiv1.InteractiveRepositoryIdentity `json:"repository"`
	Branch             string                              `json:"branch"`
	Commit             string                              `json:"commit"`
	SourceTargetDigest string                              `json:"sourceTargetDigest"`
	Files              []DocumentFileRead                  `json:"files"`
	StartOffset        int                                 `json:"startOffset"`
	TotalPaths         int                                 `json:"totalPaths"`
	NextCursor         string                              `json:"nextCursor,omitempty"`
	Exhausted          bool                                `json:"exhausted"`
	Coverage           string                              `json:"coverage"`
	Reasons            []string                            `json:"reasons,omitempty"`
}
