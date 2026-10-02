package configsignal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCacheTracksInputsWithoutReadingUnchangedContent(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "instructions.md")
	paths := []string{filepath.Join(root, "definition.yaml"), filepath.Join(root, "assets", "data"), external}
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range paths {
		write(path, "before")
	}
	var cache Cache
	calls := 0
	now := time.Now()
	hash := func(observe func(string)) (string, error) { calls++; observe(external); return fmt.Sprint(calls), nil }
	check := func() string {
		t.Helper()
		digest, err := cache.Digest(now, []string{root}, hash)
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	check()
	for i := 0; i < 20; i++ {
		if got := check(); got != "1" {
			t.Fatalf("unchanged poll read content: %s", got)
		}
	}
	for _, path := range paths {
		before := calls
		write(path, "a changed input")
		check()
		if calls != before+1 {
			t.Fatalf("missed edit of %s", path)
		}
	}
	for _, path := range []string{filepath.Join(root, "new", "nested.yaml"), external} {
		before := calls
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if path == external {
			check()
			if calls != before+1 {
				t.Fatal("missed deletion")
			}
			before = calls
		}
		write(path, "created")
		check()
		if calls != before+1 {
			t.Fatal("missed creation")
		}
	}
	before := calls
	cache.Invalidate()
	check()
	if calls != before+1 {
		t.Fatal("explicit invalidation did not read content")
	}
}

func TestCacheAuditsAliasedMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte("before"), 0644); err != nil {
		t.Fatal(err)
	}
	var cache Cache
	now := time.Now()
	hash := func(observe func(string)) (string, error) {
		observe(path)
		content, err := os.ReadFile(path)
		return string(content), err
	}
	if _, err := cache.Digest(now, nil, hash); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after!"), 0644); err != nil {
		t.Fatal(err)
	}
	// Simulate a filesystem where metadata comparison cannot distinguish the
	// edit, including ctime and identity, without relying on host granularity.
	cache.files[path], _ = os.Stat(path)
	if got, _ := cache.Digest(now.Add(AuditInterval-time.Nanosecond), nil, hash); got != "before" {
		t.Fatal(got)
	}
	if got, _ := cache.Digest(now.Add(AuditInterval), nil, hash); got != "after!" {
		t.Fatalf("audit missed hidden edit: %s", got)
	}
}

func TestCacheDetectsRestoredMtime(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("ctime signal is platform specific; periodic audit remains available")
	}
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte("before"), 0644); err != nil {
		t.Fatal(err)
	}
	var cache Cache
	hash := func(observe func(string)) (string, error) {
		observe(path)
		content, err := os.ReadFile(path)
		return string(content), err
	}
	now := time.Now()
	if _, err := cache.Digest(now, nil, hash); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("after!"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got, _ := cache.Digest(now, nil, hash); got != "after!" {
		t.Fatalf("restored mtime hid edit: %s", got)
	}
}

func TestCacheDoesNotRetainErrorsOrConcurrentEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	var cache Cache
	now := time.Now()
	fail := errors.New("unreadable")
	if _, err := cache.Digest(now, nil, func(observe func(string)) (string, error) { observe(path); return "", fail }); !errors.Is(err, fail) {
		t.Fatal(err)
	}
	if cache.files != nil {
		t.Fatal("cached error")
	}
	_, err := cache.Digest(now, nil, func(observe func(string)) (string, error) {
		observe(path)
		return "old", os.WriteFile(path, []byte("new"), 0644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if cache.files != nil {
		t.Fatal("cached concurrently changed generation")
	}
}

func BenchmarkUnchangedPoll(b *testing.B) {
	root := b.TempDir()
	for i := 0; i < 100; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%d.yaml", i)), make([]byte, 64<<10), 0644); err != nil {
			b.Fatal(err)
		}
	}
	hash := func(func(string)) (string, error) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			if _, err := os.ReadFile(filepath.Join(root, entry.Name())); err != nil {
				return "", err
			}
		}
		return "digest", nil
	}
	for _, cached := range []bool{false, true} {
		b.Run(fmt.Sprintf("cached=%t", cached), func(b *testing.B) {
			var cache Cache
			now := time.Now()
			if _, err := cache.Digest(now, []string{root}, hash); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if !cached {
					cache.Invalidate()
				}
				if _, err := cache.Digest(now, []string{root}, hash); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
