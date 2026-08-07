package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelpRoutesCommandsAndConcepts(t *testing.T) {
	t.Run("command", func(t *testing.T) {
		code, stdout, stderr := runArgs(t, "help", "init")
		if code != 0 || stdout != initHelp || stderr != "" {
			t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
	})

	t.Run("nested command", func(t *testing.T) {
		code, stdout, stderr := runArgs(t, "help", "examples", "show")
		if code != 0 || stdout != examplesShowHelp || stderr != "" {
			t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
	})

	for _, topic := range helpConceptTopics {
		t.Run(topic+" concept", func(t *testing.T) {
			code, stdout, stderr := runArgs(t, "help", topic)
			want := topic + "\n\n" + glossary[topic] + "\n"
			if command, ok := helpCommand(topic); ok {
				want = command.long
			}
			if code != 0 || stdout != want || stderr != "" {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			if !strings.Contains(stdout, glossary[topic]) {
				t.Fatalf("stdout does not include %s concept prose: %q", topic, stdout)
			}
		})
	}
}

func TestHelpUnknownTopicErrorsWithSuggestion(t *testing.T) {
	code, stdout, stderr := runArgs(t, "help", "instnace")
	if code == 0 {
		t.Fatal("unknown help topic exited successfully")
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, `unknown topic "instnace"`) ||
		!strings.Contains(stderr, `did you mean "instance"?`) {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestHelpConceptProseMatchesGlossary(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "docs", "concepts", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(content)
	for topic, prose := range glossary {
		entry := "| **" + strings.ToUpper(topic[:1]) + topic[1:] + "** | " + prose + " |"
		if !strings.Contains(doc, entry) {
			t.Errorf("glossary is missing shared %q prose", topic)
		}
	}
}

func TestUsageFooterLinksOnboardingPaths(t *testing.T) {
	_, stdout, _ := runArgs(t, "help")
	for _, line := range []string{
		"Quickstart guide: docs/guides/quickstart.md",
		"DSL entry points: `goobers schema` and `goobers examples`",
		"Troubleshooting: `goobers status`, `goobers trace`, and `goobers escalations`",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("help footer missing %q", line)
		}
	}
}
