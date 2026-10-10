package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestArchivedStageReleaseRepairsOnlyVerifiedCustody(t *testing.T) {
	for _, transition := range []string{"held", "interrupted-hold", "interrupted-release", "interrupted-cleanup", "pending", "foreign-owner", "foreign-start", "kept", "quarantined", "verification-fails"} {
		t.Run(transition, func(t *testing.T) {
			m, err := NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			url := "https://example.invalid/acme/repo"
			key := repoKey(url)
			if err := os.MkdirAll(m.repoDirForKey(key), 0700); err != nil {
				t.Fatal(err)
			}
			custody := StageCustody{WorkspaceID: "stage", OwnerRunID: "parent", RepositoryDigest: RepositoryDigest(url), Branch: "goobers/parent", StartRef: "original"}
			primary := marker{RunID: custody.WorkspaceID, OwnerRunID: custody.OwnerRunID, RepositoryDigest: custody.RepositoryDigest, Branch: custody.Branch, StartRef: custody.StartRef, Directory: worktreeDirectoryName(custody.WorkspaceID), BaseRef: "cumulative-base", Status: statusCleanupRetained, CleanupDisposition: childWaitDisposition}
			ownership := primary
			wantFailure := false
			switch transition {
			case "interrupted-hold":
				primary.Status, primary.CleanupDisposition = statusActive, ""
			case "interrupted-release":
				ownership.Status, ownership.CleanupDisposition = statusActive, ""
			case "interrupted-cleanup":
				primary.Status, primary.CleanupDisposition = statusActive, ""
				ownership.Status, ownership.CleanupDisposition = statusCleanupPending, ""
			case "pending":
				primary.Status, primary.CleanupDisposition = statusCleanupPending, ""
				ownership = primary
			case "foreign-owner":
				ownership.OwnerRunID, wantFailure = "another-run", true
			case "foreign-start":
				ownership.StartRef, wantFailure = "another-revision", true
			case "kept":
				ownership.Status, wantFailure = statusKept, true
			case "quarantined":
				ownership.CleanupDisposition, wantFailure = CleanupDispositionRetryExhausted, true
			case "verification-fails":
				wantFailure = true
			}
			primaryPath := m.markerPath(key, custody.WorkspaceID)
			ownershipPath := m.ownershipPath(key, primary.Directory)
			for path, mk := range map[string]marker{primaryPath: primary, ownershipPath: ownership} {
				if err := writeMarker(path, mk); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			verify := func(_ context.Context, target CleanupTarget) error {
				calls++
				if target.BaseRef != "cumulative-base" || target.StartRef != custody.StartRef || target.OwnerRunID != custody.OwnerRunID {
					t.Fatal("release changed cumulative custody", target)
				}
				if transition == "verification-fails" {
					return errors.New("archive unavailable")
				}
				return nil
			}
			err = m.ReleaseArchivedStage(t.Context(), url, custody, verify)
			if (err != nil) != wantFailure {
				t.Fatal("release result", err)
			}
			for path, original := range map[string]marker{primaryPath: primary, ownershipPath: ownership} {
				got, err := readMarker(path)
				if err != nil {
					t.Fatal(err)
				}
				if wantFailure && got.Status != original.Status || wantFailure && got.CleanupDisposition != original.CleanupDisposition {
					t.Fatal("failed release changed markers", got)
				}
				if !wantFailure && (got.Status != statusCleanupPending || got.CleanupDisposition != "") {
					t.Fatal("release did not surrender cleanup", got)
				}
			}
			if wantFailure {
				return
			}
			if err := m.ReleaseArchivedStage(t.Context(), url, custody, verify); err != nil || calls != 2 {
				t.Fatal("retry skipped archive verification", calls, err)
			}
			for _, path := range []string{primaryPath, ownershipPath} {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.ReleaseArchivedStage(t.Context(), url, custody, verify); err != nil || calls != 2 {
				t.Fatal("already removed checkout did not settle", err)
			}
			path := filepath.Join(m.runsDirForKey(key), primary.Directory)
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := m.ReleaseArchivedStage(t.Context(), url, custody, verify); err == nil {
				t.Fatal("unowned surviving directory accepted")
			}
		})
	}
}
