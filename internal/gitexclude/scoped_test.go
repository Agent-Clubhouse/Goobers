package gitexclude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestScopedExclusionsPreserveConcurrentOwnersAndEnsure(t *testing.T) {
	ctx, repo := context.Background(), initRepo(t)
	exclude := filepath.Join(repo, ".git/info/exclude")
	original := "# operator\n/build-output"
	writeFile(t, exclude, original)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("session-%d", i)
			if err := AddScoped(ctx, repo, id, Pattern{Line: "/.github/skills/review"}); err != nil {
				t.Error(err)
				return
			}
			if err := Ensure(ctx, repo, Pattern{Line: "/persistent"}); err != nil {
				t.Error(err)
			}
			if err := RemoveScoped(ctx, repo, id); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), original) || strings.Count(string(data), "/persistent") != 1 || strings.Contains(string(data), "scoped") {
		t.Fatalf("lost or leaked exclusions: %q", data)
	}
}

func TestScopedExclusionsRestoreUnterminatedContent(t *testing.T) {
	repo := initRepo(t)
	exclude := filepath.Join(repo, ".git/info/exclude")
	original := "# operator\n/build-output"
	writeFile(t, exclude, original)
	ctx := context.Background()
	for _, id := range []string{"one", "two"} {
		if err := AddScoped(ctx, repo, id, Pattern{Line: "/same"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"one", "one", "two"} {
		if err := RemoveScoped(ctx, repo, id); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(exclude)
	if string(data) != original {
		t.Fatalf("restore = %q", data)
	}
}
