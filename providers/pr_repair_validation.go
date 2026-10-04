package providers

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"
)

var repairCommandID = regexp.MustCompile(`^repair-[0-9a-f]{32}$`)

// ValidatePullRequestRepair checks bounded retained intent without provider work.
func ValidatePullRequestRepair(value PullRequestRepair) error {
	if !validRepairTarget(value.Target) || !value.Target.Open || !repairCommandID.MatchString(value.CommandID) || !nativeEditText(value.Message, 8192, false) || !strings.Contains(value.Message, "\n\n") || !strings.Contains(value.Message, "\nGoobers-Repair: "+value.CommandID+"\n") || len(value.Changes) == 0 || len(value.Changes) > MaxPRRepairFiles {
		return ErrPRRepair
	}
	bytes := 0
	for i, change := range value.Changes {
		if !validRepairPath(change.Path) || (change.PreviousBlob != "" && !ValidSourceCommit(change.PreviousBlob)) || (change.PreviousBlob == "" && change.Content == nil) {
			return ErrPRRepair
		}
		if change.Content != nil {
			bytes += len(*change.Content)
			if !utf8.ValidString(*change.Content) || strings.ContainsRune(*change.Content, 0) || bytes > MaxPRRepairContentBytes {
				return ErrPRRepair
			}
		}
		for _, prior := range value.Changes[:i] {
			a, b := strings.ToLower(change.Path), strings.ToLower(prior.Path)
			if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
				return ErrPRRepair
			}
		}
	}
	return nil
}

type repairFileReader func(context.Context, RepositoryRef, string, string) (RepairFile, error)

func validateRepairBefore(ctx context.Context, value PullRequestRepair, reader repairFileReader) error {
	for _, change := range value.Changes {
		file, err := reader(ctx, value.Target.Repository, value.Target.HeadSHA, change.Path)
		if err != nil {
			return err
		}
		if file.Present != (change.PreviousBlob != "") || file.BlobID != change.PreviousBlob {
			return ErrPRRepair
		}
		// Native file-addition inputs have no mode field. Preserve executable
		// files by refusing their modification until a mode-aware path exists.
		if change.Content != nil && file.Present && (file.Mode != "100644" || file.BlobID == sourceBlobID([]byte(*change.Content))) {
			return ErrPRRepair
		}
	}
	return nil
}
func verifyRepairFiles(ctx context.Context, value PullRequestRepair, commit string, reader repairFileReader) error {
	for _, change := range value.Changes {
		before, err := reader(ctx, value.Target.Repository, value.Target.HeadSHA, change.Path)
		if err != nil {
			return err
		}
		after, err := reader(ctx, value.Target.Repository, commit, change.Path)
		if err != nil {
			return err
		}
		if before.Present != (change.PreviousBlob != "") || before.BlobID != change.PreviousBlob {
			return ErrPRRepair
		}
		if change.Content == nil {
			if after.Present {
				return ErrPRRepair
			}
			continue
		}
		mode := "100644"
		if before.Present {
			mode = before.Mode
		}
		if !after.Present || after.Mode != mode || after.BlobID != sourceBlobID([]byte(*change.Content)) || after.Content != *change.Content {
			return ErrPRRepair
		}
	}
	return nil
}
func repairChangedPaths(value PullRequestRepair, paths []string) bool {
	if len(paths) != len(value.Changes) {
		return false
	}
	seen := map[string]bool{}
	for _, path := range paths {
		if seen[path] {
			return false
		}
		seen[path] = true
	}
	for _, change := range value.Changes {
		if !seen[change.Path] {
			return false
		}
	}
	return true
}
