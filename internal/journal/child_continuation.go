package journal

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// prepareContinuationIdentity copies only the admitted identity/context. It
// never edits the source, inherits ambient context or changes accepted pins.
func prepareContinuationIdentity(id RunIdentity, req ContinuationRequest, recordedBranch, recordedSHA string) (RunIdentity, error) {
	var err error
	id.Child, err = childContinuationLineage(id, req.ChildContinuation)
	if err != nil {
		return RunIdentity{}, err
	}
	id.RunID = req.RunID
	id.Event = nil
	id.ContinuedFromRunID = req.SourceRunID
	id.SourceTerminalSeq = req.ExpectedTerminalSeq
	id.Operator = strings.TrimSpace(req.Operator)
	id.RequestedTarget = strings.TrimSpace(req.Target)
	id.WorkspaceBranch = strings.TrimSpace(req.SourceBranch)
	id.WorkspaceBranchSHA = strings.TrimSpace(req.ExpectedSourceSHA)
	if req.SourceRepository != nil {
		repository := *req.SourceRepository
		id.WorkspaceRepository = &repository
	}
	if id.WorkspaceBranch == "" {
		id.WorkspaceBranch = recordedBranch
	}
	if id.WorkspaceBranchSHA == "" {
		id.WorkspaceBranchSHA = recordedSHA
	}
	// The request is the complete allowlist; never inherit ambient source context.
	id.ContextPointers = make([]apiv1.ContextPointer, len(req.ContextPointers))
	copy(id.ContextPointers, req.ContextPointers)
	for i := range id.ContextPointers {
		p := &id.ContextPointers[i]
		if p.RunID == "" {
			p.RunID = req.SourceRunID
		}
		if p.Artifact == nil || p.External != nil {
			return RunIdentity{}, fmt.Errorf("journal: continuation context pointer %q is not an explicit source artifact", p.Name)
		}
		if err := p.Validate(); err != nil {
			return RunIdentity{}, fmt.Errorf("journal: invalid continuation context pointer %q: %w", p.Name, err)
		}
	}
	return id, nil
}
