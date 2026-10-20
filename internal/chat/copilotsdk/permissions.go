package copilotsdk

import (
	"github.com/github/copilot-sdk/go/rpc"
	"github.com/goobers/goobers/internal/chat"
)

// Only explicit display fields cross this boundary. Never serialize RPC params,
// custom tool arguments, environment entries, or opaque permission metadata.
func permissionPreview(request rpc.PermissionRequest) ([]chat.ContentBlock, string) {
	var text string
	var toolCallID *string
	switch req := request.(type) {
	case *rpc.PermissionRequestShell:
		text, toolCallID = req.FullCommandText, req.ToolCallID
	case *rpc.PermissionRequestRead:
		text, toolCallID = req.Path, req.ToolCallID
	case *rpc.PermissionRequestWrite:
		text, toolCallID = req.FileName+"\n"+req.Diff, req.ToolCallID
		if req.NewFileContents != nil {
			text += "\n" + *req.NewFileContents
		}
	case *rpc.PermissionRequestURL:
		text, toolCallID = req.URL, req.ToolCallID
	}
	id := ""
	if toolCallID != nil {
		id = *toolCallID
	}
	return reviewText(text), id
}

func reviewText(text string) []chat.ContentBlock {
	// An incomplete preview must not look sufficient to approve. The dock keeps
	// requests without reviewable content deny-only.
	if text == "" || len(text) > 32<<10 {
		return []chat.ContentBlock{}
	}
	return chat.Text(text)
}
