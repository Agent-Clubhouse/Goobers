package journal

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ChildContinuationWorkspace is host-only admission of a fresh managed fork.
// The queue's sealed prior result is verified before supplying these selectors.
// It changes no repository, workflow or capability pin.
type ChildContinuationWorkspace struct {
	Branch  string
	ForkSHA string
}

var childContinuationSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func bindChildContinuationWorkspace(id *RunIdentity, req ContinuationRequest) error {
	if req.ChildWorkspace == nil {
		if id.Child != nil && strings.HasPrefix(id.WorkspaceBranch, "goobers/children/") {
			return fmt.Errorf("journal: repository child continuation requires a new admitted fork")
		}
		return nil
	}
	fork := req.ChildWorkspace
	if id.Child == nil || req.ChildContinuation == nil || id.Child.ExecutionEpoch < 1 || id.WorkspaceRepository == nil || fork.Branch != "goobers/children/"+req.RunID || !childContinuationSHA.MatchString(fork.ForkSHA) {
		return fmt.Errorf("journal: invalid child continuation fork identity")
	}
	id.WorkspaceBranch, id.WorkspaceBranchSHA = fork.Branch, fork.ForkSHA
	return nil
}

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
		if id.Child != nil && !reflect.DeepEqual(id.WorkspaceRepository, req.SourceRepository) {
			return RunIdentity{}, fmt.Errorf("journal: child continuation cannot change repository")
		}
		repository := *req.SourceRepository
		id.WorkspaceRepository = &repository
	}
	if id.WorkspaceBranch == "" {
		id.WorkspaceBranch = recordedBranch
	}
	if id.WorkspaceBranchSHA == "" {
		id.WorkspaceBranchSHA = recordedSHA
	}
	if err := bindChildContinuationWorkspace(&id, req); err != nil {
		return RunIdentity{}, err
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
