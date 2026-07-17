package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

// fakeMergePolicyProvider is the narrow RepoProvider slice
// detectMergePolicy depends on, stubbed via the same embedded-nil-interface
// pattern internal/mergepolicy's own tests use.
type fakeMergePolicyProvider struct {
	providers.RepoProvider
	calls  int
	result providers.RepoMergePolicyResult
	err    error
}

func (f *fakeMergePolicyProvider) DetectMergePolicy(ctx context.Context, req providers.RepoMergePolicyRequest) (providers.RepoMergePolicyResult, error) {
	f.calls++
	return f.result, f.err
}

func TestDetectMergePolicyCachesAcrossCalls(t *testing.T) {
	root := initDemo(t)
	l := layoutFor(root)
	repo := providers.RepositoryRef{Owner: "acme", Name: "widgets"}
	fake := &fakeMergePolicyProvider{result: providers.RepoMergePolicyResult{Policy: providers.MergePolicyMergeQueue}}
	var stderr bytes.Buffer

	policy, err := detectMergePolicy(context.Background(), fake, l.SchedulerDir(), repo, "main", &stderr)
	if err != nil {
		t.Fatalf("first detectMergePolicy call: %v", err)
	}
	if policy != providers.MergePolicyMergeQueue {
		t.Fatalf("policy = %q, want %q", policy, providers.MergePolicyMergeQueue)
	}
	if fake.calls != 1 {
		t.Fatalf("provider called %d times after first detect, want 1", fake.calls)
	}

	// A second call for the SAME repo+branch, still within TTL, must hit the
	// cache — not call the provider again.
	policy, err = detectMergePolicy(context.Background(), fake, l.SchedulerDir(), repo, "main", &stderr)
	if err != nil {
		t.Fatalf("second detectMergePolicy call: %v", err)
	}
	if policy != providers.MergePolicyMergeQueue {
		t.Fatalf("cached policy = %q, want %q", policy, providers.MergePolicyMergeQueue)
	}
	if fake.calls != 1 {
		t.Fatalf("provider called %d times after second detect, want still 1 (cache hit)", fake.calls)
	}
}

func TestDetectMergePolicyMissesCacheForDifferentBranch(t *testing.T) {
	root := initDemo(t)
	l := layoutFor(root)
	repo := providers.RepositoryRef{Owner: "acme", Name: "widgets"}
	fake := &fakeMergePolicyProvider{result: providers.RepoMergePolicyResult{Policy: providers.MergePolicyDirect}}
	var stderr bytes.Buffer

	if _, err := detectMergePolicy(context.Background(), fake, l.SchedulerDir(), repo, "main", &stderr); err != nil {
		t.Fatalf("detect for main: %v", err)
	}
	if _, err := detectMergePolicy(context.Background(), fake, l.SchedulerDir(), repo, "release", &stderr); err != nil {
		t.Fatalf("detect for release: %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("provider called %d times, want 2 (distinct branches must not share a cache entry)", fake.calls)
	}
}

func TestDetectMergePolicyExpiredEntryReDetects(t *testing.T) {
	root := initDemo(t)
	l := layoutFor(root)
	repo := providers.RepositoryRef{Owner: "acme", Name: "widgets"}
	key := mergePolicyCacheKey(repo, "main")

	// Seed an expired entry directly (bypassing detectMergePolicy) so this
	// test does not depend on real wall-clock sleeping.
	stale := mergePolicyCacheEntry{Policy: providers.MergePolicyMergeQueue, DetectedAt: time.Now().Add(-2 * mergePolicyCacheTTL)}
	if err := saveMergePolicyCacheEntry(l.SchedulerDir(), key, stale); err != nil {
		t.Fatalf("seed stale cache entry: %v", err)
	}

	fake := &fakeMergePolicyProvider{result: providers.RepoMergePolicyResult{Policy: providers.MergePolicyDirect}}
	var stderr bytes.Buffer
	policy, err := detectMergePolicy(context.Background(), fake, l.SchedulerDir(), repo, "main", &stderr)
	if err != nil {
		t.Fatalf("detectMergePolicy: %v", err)
	}
	if policy != providers.MergePolicyDirect {
		t.Fatalf("policy = %q, want %q (live re-detection, not the stale cached merge_queue)", policy, providers.MergePolicyDirect)
	}
	if fake.calls != 1 {
		t.Fatalf("provider called %d times, want 1 (an expired entry must re-detect)", fake.calls)
	}
}

func TestDetectMergePolicyPropagatesProviderError(t *testing.T) {
	root := initDemo(t)
	l := layoutFor(root)
	repo := providers.RepositoryRef{Owner: "acme", Name: "widgets"}
	wantErr := errors.New("rate limited")
	fake := &fakeMergePolicyProvider{err: wantErr}
	var stderr bytes.Buffer

	_, err := detectMergePolicy(context.Background(), fake, l.SchedulerDir(), repo, "main", &stderr)
	if !errors.Is(err, wantErr) {
		t.Fatalf("detectMergePolicy error = %v, want %v", err, wantErr)
	}
}

func TestMergePolicyCacheCorruptFileDegradesToLiveDetect(t *testing.T) {
	root := initDemo(t)
	l := layoutFor(root)
	repo := providers.RepositoryRef{Owner: "acme", Name: "widgets"}

	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(l.SchedulerDir(), mergePolicyCacheFileName), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write corrupt cache: %v", err)
	}

	fake := &fakeMergePolicyProvider{result: providers.RepoMergePolicyResult{Policy: providers.MergePolicyDirect}}
	var stderr bytes.Buffer
	policy, err := detectMergePolicy(context.Background(), fake, l.SchedulerDir(), repo, "main", &stderr)
	if err != nil {
		t.Fatalf("detectMergePolicy: %v", err)
	}
	if policy != providers.MergePolicyDirect {
		t.Fatalf("policy = %q, want %q", policy, providers.MergePolicyDirect)
	}
	if stderr.Len() == 0 {
		t.Fatal("want a warning printed for the unreadable cache")
	}
}
