package publicationpaths

import "testing"

func TestIsReserved(t *testing.T) {
	for _, path := range []string{
		".goobers", ".goobers/prompt.md", ".goobers/mcp/home/logs/debug.log",
		".goober-assets", ".goober-assets/skill.md", "mutations.jsonl", "mutations.jsonl/entry",
		".goobers-launcher-session-", ".goobers-launcher-session-123", ".goobers-launcher-session-123/native.jsonl",
	} {
		if !IsReserved(path) {
			t.Errorf("IsReserved(%q) = false", path)
		}
	}
	for _, path := range []string{
		"", "src/main.go", ".goobers-config", ".goober-assets.md", "mutations.jsonl.bak",
		"project/.goobers/prompt.md", "project/.goober-assets/skill.md", "project/mutations.jsonl",
		"/.goobers/prompt.md", "../.goobers/prompt.md", `.goobers\prompt.md`,
		".goobers-launcher-session", ".goobers-launcher-session.md", ".goobers-launcher-settings",
		"project/.goobers-launcher-session-123/native.jsonl", "./.goobers-launcher-session-123/native.jsonl",
	} {
		if IsReserved(path) {
			t.Errorf("IsReserved(%q) = true", path)
		}
	}
}

func TestExcludePatternsAreRootAnchoredAndIndependent(t *testing.T) {
	patterns := ExcludePatterns()
	for _, pattern := range patterns {
		if len(pattern.Line) < 2 || pattern.Line[0] != '/' || !IsReserved(pattern.Line[1:]) || len(pattern.Aliases) != 0 {
			t.Errorf("exclude pattern disagrees with reserved root policy: %+v", pattern)
		}
	}
	if got := patterns[len(patterns)-1].Line; got != "/.goobers-launcher-session-*" {
		t.Errorf("launcher-session exclusion = %q, want exact root prefix glob", got)
	}
	patterns[0].Line = "changed"
	if ExcludePatterns()[0].Line != "/.goobers" {
		t.Fatal("caller changed the shared exclusion policy")
	}
}
