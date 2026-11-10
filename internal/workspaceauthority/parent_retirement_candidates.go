package workspaceauthority

import (
	"encoding/json"
	"errors"

	"github.com/goobers/goobers/internal/journal"
)

// ParentEventCustody reads workspace custody from a contained-return or fork-ready event.
func ParentEventCustody(event journal.Event) (ContainedParentWorkspaceCustody, bool, error) {
	var custody ContainedParentWorkspaceCustody
	if event.Type != journal.EventRunnerAnnotation {
		return custody, false, nil
	}
	if event.Runner["kind"] == ParentForkReadyKind || event.Runner["kind"] == ParentForkRootReadyKind {
		fork, err := decodeParentForkCustody(event)
		return ContainedParentWorkspaceCustody{Version: 1, Workspace: fork.Workspace}, true, err
	}
	if event.Runner["kind"] != ContainedParentWorkspaceKind {
		return custody, false, nil
	}
	data, err := json.Marshal(event.Runner["custody"])
	if err != nil || len(data) > 8192 || json.Unmarshal(data, &custody) != nil || custody.Version != 1 || custody.Origin == nil || custody.Workspace.Branch == "" {
		return custody, true, errors.New("invalid parent retirement custody")
	}
	return custody, true, nil
}
