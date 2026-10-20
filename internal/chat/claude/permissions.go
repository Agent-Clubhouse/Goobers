package claude

import (
	"strings"

	"github.com/goobers/goobers/internal/chat"
)

func permissionPreview(tool string, input object) []chat.ContentBlock {
	var keys []string
	switch tool {
	case "Bash":
		keys = []string{"command"}
	case "Read", "Glob", "Grep":
		keys = []string{"file_path", "path", "pattern"}
	case "Write":
		keys = []string{"file_path", "content"}
	case "Edit":
		keys = []string{"file_path", "old_string", "new_string"}
	case "WebFetch":
		keys = []string{"url"}
	default:
		// Opaque tool inputs are not safe to copy into the event journal.
		return []chat.ContentBlock{}
	}
	var lines []string
	for _, key := range keys {
		if value, ok := input[key].(string); ok && value != "" {
			lines = append(lines, key+": "+value)
		}
	}
	text := strings.Join(lines, "\n")
	if text == "" || len(text) > 32<<10 {
		return []chat.ContentBlock{}
	}
	return chat.Text(text)
}
