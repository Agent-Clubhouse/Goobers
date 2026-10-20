package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestProjectRunReportsPublishedDirectoryOnFinalizationFailure(t *testing.T) {
	spec := crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)
	proj := executeForProjection(t, projectionInput("publication-error", spec), &Activities{
		Det: &scriptedStages{}, Workspaces: testWorkspaces(t),
	}, false)
	runsDir := filepath.Join(t.TempDir(), "runs")
	failure := errors.New("directory sync failed after publication")
	option := func(cfg *projectConfig) {
		cfg.replaceRun = func(finalDir, stagedDir string, keep func() (bool, error)) (bool, error) {
			published, err := journal.ReplaceRun(finalDir, stagedDir, keep)
			if err != nil || !published {
				t.Fatalf("publish fixture: %v, %v", published, err)
			}
			return true, failure
		}
	}
	dir, err := ProjectRun(runsDir, proj, option)
	if dir != filepath.Join(runsDir, proj.Identity.RunID) || !errors.Is(err, failure) || !strings.Contains(err.Error(), "was published") {
		t.Fatalf("ProjectRun = %q, %v", dir, err)
	}
	if complete, err := projectedJournalComplete(dir); err != nil || !complete {
		t.Fatalf("published projection lost on error: complete=%v err=%v", complete, err)
	}
}
