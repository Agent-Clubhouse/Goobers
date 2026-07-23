package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

func TestCalculateBacklogStalenessUsesHumanActivity(t *testing.T) {
	observedAt := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	createdAt := observedAt.Add(-40 * 24 * time.Hour)
	recentBot := observedAt.Add(-time.Hour)
	recentHuman := observedAt.Add(-5 * 24 * time.Hour)
	policy := backlogStalenessPolicy{thresholdDays: 30}

	tests := []struct {
		name         string
		comments     []providers.Comment
		wantStale    bool
		wantAgeDays  int
		wantActivity time.Time
	}{
		{
			name: "bot activity does not reset clock",
			comments: []providers.Comment{{
				Author: "goobers", AuthorType: "Bot", CreatedAt: &recentBot,
			}},
			wantStale:    true,
			wantAgeDays:  40,
			wantActivity: createdAt,
		},
		{
			name: "human activity resets clock",
			comments: []providers.Comment{{
				Author: "maintainer", AuthorType: "User", CreatedAt: &recentHuman,
			}},
			wantStale:    false,
			wantAgeDays:  5,
			wantActivity: recentHuman,
		},
		{
			name: "authenticated automation user does not reset clock",
			comments: []providers.Comment{{
				Author: "goobers", AuthorType: "User", CreatedAt: &recentBot,
			}},
			wantStale:    true,
			wantAgeDays:  40,
			wantActivity: createdAt,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signal, err := calculateBacklogStaleness(
				providers.WorkItem{CreatedAt: &createdAt},
				tt.comments,
				"goobers",
				observedAt,
				policy,
			)
			if err != nil {
				t.Fatalf("calculateBacklogStaleness: %v", err)
			}
			if signal.Stale != tt.wantStale {
				t.Errorf("Stale = %v, want %v", signal.Stale, tt.wantStale)
			}
			if signal.AgeDays != tt.wantAgeDays {
				t.Errorf("AgeDays = %d, want %d", signal.AgeDays, tt.wantAgeDays)
			}
			if signal.ThresholdDays != 30 {
				t.Errorf("ThresholdDays = %d, want 30", signal.ThresholdDays)
			}
			if !signal.LastMeaningfulActivityAt.Equal(tt.wantActivity) {
				t.Errorf(
					"LastMeaningfulActivityAt = %s, want %s",
					signal.LastMeaningfulActivityAt,
					tt.wantActivity,
				)
			}
		})
	}
}

func TestCalculateBacklogStalenessFlagsAtThreshold(t *testing.T) {
	observedAt := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	lastActivity := observedAt.Add(-30 * 24 * time.Hour)

	signal, err := calculateBacklogStaleness(
		providers.WorkItem{CreatedAt: &lastActivity},
		nil,
		"goobers",
		observedAt,
		backlogStalenessPolicy{thresholdDays: 30, autoCloseStale: true},
	)
	if err != nil {
		t.Fatalf("calculateBacklogStaleness: %v", err)
	}
	if !signal.Stale {
		t.Error("Stale = false, want true at the configured threshold")
	}
	if !signal.AutoCloseEnabled {
		t.Error("AutoCloseEnabled = false, want configured true")
	}
}

func TestReadBacklogStalenessPolicyValidatesInputs(t *testing.T) {
	t.Run("conservative defaults", func(t *testing.T) {
		t.Setenv("GOOBERS_INPUT_STALEAFTERDAYS", "")
		t.Setenv("GOOBERS_INPUT_STALEAUTOCLOSE", "")
		policy, err := readBacklogStalenessPolicy()
		if err != nil {
			t.Fatalf("readBacklogStalenessPolicy: %v", err)
		}
		if policy.thresholdDays != 90 || policy.autoCloseStale {
			t.Fatalf("policy = %+v, want thresholdDays=90 autoCloseStale=false", policy)
		}
	})

	t.Run("configured", func(t *testing.T) {
		t.Setenv("GOOBERS_INPUT_STALEAFTERDAYS", "45")
		t.Setenv("GOOBERS_INPUT_STALEAUTOCLOSE", "true")
		policy, err := readBacklogStalenessPolicy()
		if err != nil {
			t.Fatalf("readBacklogStalenessPolicy: %v", err)
		}
		if policy.thresholdDays != 45 || !policy.autoCloseStale {
			t.Fatalf("policy = %+v, want thresholdDays=45 autoCloseStale=true", policy)
		}
	})

	tests := []struct {
		name      string
		days      string
		autoClose string
		wantError string
	}{
		{name: "zero days", days: "0", autoClose: "false", wantError: "invalid staleAfterDays"},
		{name: "non-numeric days", days: "soon", autoClose: "false", wantError: "invalid staleAfterDays"},
		{name: "invalid auto-close", days: "90", autoClose: "yes", wantError: "invalid staleAutoClose"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GOOBERS_INPUT_STALEAFTERDAYS", tt.days)
			t.Setenv("GOOBERS_INPUT_STALEAUTOCLOSE", tt.autoClose)
			_, err := readBacklogStalenessPolicy()
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error = %v, want %q", err, tt.wantError)
			}
		})
	}
}

func TestBacklogQueryCurationWritesStructuredStaleness(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Old item", "goobers:approved")
	server.addIssue(8, "Recently discussed item", "goobers:approved")

	now := time.Now().UTC()
	oldActivity := now.Add(-45 * 24 * time.Hour)
	recentBot := now.Add(-time.Hour)
	recentHuman := now.Add(-5 * 24 * time.Hour)
	server.mu.Lock()
	server.issues[7].createdAt = oldActivity
	server.issues[7].comments = []string{"automated bookkeeping"}
	server.issues[7].commentIDs = []int64{1}
	server.issues[7].commentAuthors = []string{"goobers"}
	server.issues[7].commentTypes = []string{"Bot"}
	server.issues[7].commentTimes = []time.Time{recentBot}
	server.issues[8].createdAt = oldActivity
	server.issues[8].comments = []string{"still interested"}
	server.issues[8].commentIDs = []int64{2}
	server.issues[8].commentAuthors = []string{"maintainer"}
	server.issues[8].commentTypes = []string{"User"}
	server.issues[8].commentTimes = []time.Time{recentHuman}
	server.nextCommentID = 2
	server.mu.Unlock()

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "20")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "claimed-items.json")
	t.Setenv("GOOBERS_INPUT_STALEAFTERDAYS", "30")
	t.Setenv("GOOBERS_INPUT_STALEAUTOCLOSE", "false")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "claimed-items.json"))
	if err != nil {
		t.Fatalf("read claimed-items.json: %v", err)
	}
	var claimed []curationClaimedItem
	if err := json.Unmarshal(data, &claimed); err != nil {
		t.Fatalf("unmarshal claimed-items.json: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed items = %d, want 2", len(claimed))
	}
	byID := make(map[string]backlogStalenessSignal, len(claimed))
	for _, item := range claimed {
		byID[item.ID] = item.Staleness
	}

	old := byID["7"]
	if !old.Stale || old.ThresholdDays != 30 || old.AutoCloseEnabled {
		t.Errorf("old item staleness = %+v, want stale at 30 days with auto-close disabled", old)
	}
	if !old.LastMeaningfulActivityAt.Equal(oldActivity) {
		t.Errorf("old item last activity = %s, want %s", old.LastMeaningfulActivityAt, oldActivity)
	}
	recent := byID["8"]
	if recent.Stale || recent.AgeDays > 5 {
		t.Errorf("recent item staleness = %+v, want not stale at about 5 days", recent)
	}
	if !recent.LastMeaningfulActivityAt.Equal(recentHuman) {
		t.Errorf("recent item last activity = %s, want %s", recent.LastMeaningfulActivityAt, recentHuman)
	}
}
