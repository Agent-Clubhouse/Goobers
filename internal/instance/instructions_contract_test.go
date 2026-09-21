package instance

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const scratchInstruction = ".goobers/scratch/"

func TestShippedInstructionTemplatesDeclareScratchLocation(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	sources := []string{
		"config-examples",
		"reference-workflows",
		filepath.Join("internal", "instance", "starter"),
		filepath.Join("internal", "instance", "quickstart-v1"),
	}
	found := 0
	for _, source := range sources {
		err := fs.WalkDir(os.DirFS(repoRoot), source, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || entry.Name() != "instructions.md" {
				return nil
			}
			found++
			assertScratchInstruction(t, filepath.Join(repoRoot, filepath.FromSlash(path)))
			return nil
		})
		if err != nil {
			t.Fatalf("walk shipped instruction templates under %s: %v", source, err)
		}
	}
	if found == 0 {
		t.Fatal("no shipped instruction templates found")
	}
}

func TestGeneratedInstructionTemplatesDeclareScratchLocation(t *testing.T) {
	t.Run("guided", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "guided")
		_, err := InitGuided(root, GuidedOptions{
			GaggleName:           "widget-service",
			DisplayName:          "acme/widget-service",
			RepoOwner:            "acme",
			RepoName:             "widget-service",
			RepoTokenEnv:         "WIDGET_REPO_TOKEN",
			WorkTrackingTokenEnv: "WIDGET_ISSUES_TOKEN",
			PullRequestTokenEnv:  "WIDGET_PR_TOKEN",
			RepoPushTokenEnv:     "WIDGET_PUSH_TOKEN",
			CopilotTokenEnv:      "WIDGET_COPILOT_TOKEN",
			Workflows:            guidedWorkflowOrder,
			CICommand:            []string{"npm", "run", "ci"},
			RequiredCapabilities: []string{"node@20"},
		})
		if err != nil {
			t.Fatalf("InitGuided: %v", err)
		}
		assertGeneratedScratchInstructions(t, NewLayout(root).ConfigDir())
	})

	t.Run("quickstart", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "quickstart")
		if _, err := InitQuickstart(root); err != nil {
			t.Fatalf("InitQuickstart: %v", err)
		}
		assertGeneratedScratchInstructions(t, NewLayout(root).ConfigDir())
	})
}

func assertGeneratedScratchInstructions(t *testing.T, root string) {
	t.Helper()
	found := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "instructions.md" {
			return nil
		}
		found++
		assertScratchInstruction(t, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk generated instruction templates: %v", err)
	}
	if found == 0 {
		t.Fatal("generated config contains no instruction templates")
	}
}

func assertScratchInstruction(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), scratchInstruction) {
		t.Errorf("%s does not declare scratch files belong under %s", path, scratchInstruction)
	}
}
