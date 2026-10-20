package prstatus

import (
	"context"
	"errors"
	"testing"

	"github.com/goobers/goobers/providers"
)

type baseReader struct {
	files      []providers.ChangedFile
	comparison providers.CompareResult
	err        error
}

func (r baseReader) PullRequestFiles(context.Context, providers.RepositoryRef, string) ([]providers.ChangedFile, error) {
	return r.files, nil
}
func (r baseReader) CompareCommits(context.Context, providers.RepositoryRef, string, string) (providers.CompareResult, error) {
	return r.comparison, r.err
}
func TestBaseMovementEvidence(t *testing.T) {
	for _, tc := range []struct {
		name               string
		reader             baseReader
		intersects, failed bool
	}{
		{name: "disjoint", reader: baseReader{files: []providers.ChangedFile{{Path: "a"}}, comparison: providers.CompareResult{MergeBaseSHA: "old", Files: []providers.ChangedFile{{Path: "b"}}}}},
		{name: "rename source", reader: baseReader{files: []providers.ChangedFile{{Path: "new", PreviousPath: "a"}}, comparison: providers.CompareResult{Files: []providers.ChangedFile{{Path: "a"}}}}, intersects: true},
		{name: "divergence", reader: baseReader{comparison: providers.CompareResult{MergeBaseSHA: "ancestor"}}, intersects: true},
		{name: "incomplete", reader: baseReader{comparison: providers.CompareResult{Files: make([]providers.ChangedFile, 300)}}, failed: true},
		{name: "read error", reader: baseReader{err: errors.New("unavailable")}, failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BaseMovementIntersectsPR(t.Context(), tc.reader, providers.RepositoryRef{Provider: providers.ProviderGitHub}, "7", "old", "new")
			if got != tc.intersects || (err != nil) != tc.failed {
				t.Fatalf("got=%t err=%v", got, err)
			}
		})
	}
}
