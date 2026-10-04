package sessioning

// PRRepairInspection is an exact selected or confirmed-descendant head. It is
// observational data; host command custody remains the source of write authority.
type PRRepairInspection struct {
	Target          PRRepairTarget `json:"target"`
	ParentCommandID string         `json:"parentCommandId,omitempty"`
	HeadSHA         string         `json:"headSha"`
	BaseSHA         string         `json:"baseSha"`
	Head            string         `json:"head"`
	Base            string         `json:"base"`
	Title           string         `json:"title"`
	Description     string         `json:"description"`
	URL             string         `json:"url"`
	Open            bool           `json:"open"`
	Draft           bool           `json:"draft"`
}

// PRRepairFile carries immutable blob evidence for a literal repository path.
// Content redaction is refused, rather than presented as an editable exact blob.
type PRRepairFile struct {
	Target          PRRepairTarget `json:"target"`
	ParentCommandID string         `json:"parentCommandId,omitempty"`
	HeadSHA         string         `json:"headSha"`
	Path            string         `json:"path"`
	Present         bool           `json:"present"`
	BlobID          string         `json:"blobId,omitempty"`
	Mode            string         `json:"mode,omitempty"`
	Content         string         `json:"content,omitempty"`
}
