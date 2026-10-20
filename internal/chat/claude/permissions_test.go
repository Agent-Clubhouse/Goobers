package claude

import (
	"strings"
	"testing"
)

func TestPermissionPreviewDoesNotCopyOpaqueInput(t *testing.T) {
	content := permissionPreview("Bash", object{"command": "go test ./...", "env": object{"TOKEN": "must-not-copy"}})
	if len(content) != 1 || content[0].Text != "command: go test ./..." {
		t.Fatalf("command preview = %+v", content)
	}
	content = permissionPreview("Edit", object{"file_path": "a.go", "old_string": "old", "new_string": "new"})
	if len(content) != 1 || !strings.Contains(content[0].Text, "new_string: new") {
		t.Fatalf("edit preview = %+v", content)
	}
	if len(permissionPreview("UnknownTool", object{"secret": "must-not-copy"})) != 0 {
		t.Fatal("opaque input became review content")
	}
}
