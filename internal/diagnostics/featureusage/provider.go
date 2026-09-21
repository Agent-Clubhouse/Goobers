package featureusage

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/lock"
)

const providerFile = "feature-provider-counts.json"

// BeginProviderWindow initializes one stage's bounded provider observation.
// The stage telemetry directory is already private and provisioned by executor.
func BeginProviderWindow(dir string) {
	if !plainDirectory(dir) {
		return
	}
	path := filepath.Join(dir, providerFile)
	info, err := os.Lstat(path)
	if err == nil && !info.Mode().IsRegular() {
		return
	}
	_ = os.WriteFile(path, []byte("{}"), 0o600)
}
func plainDirectory(dir string) bool {
	if dir == "" {
		return false
	}
	info, err := os.Lstat(dir)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

// RecordProviderHTTP counts a concrete Goobers provider HTTP attempt. It does
// not inspect endpoint, headers, query, body, owner or credentials. The stage
// sidecar is absent outside instrumented stage execution, making this a no-op.
// Lock acquisition never waits; dropped observations yield partial coverage.
func RecordProviderHTTP(provider string) {
	id := ""
	switch provider {
	case "github":
		id = "provider.github"
	case "gitea":
		id = "provider.gitea"
	case "ado":
		id = "provider.azure-devops"
	}
	dir := os.Getenv("GOOBERS_TELEMETRY_DIR")
	if id == "" || !plainDirectory(dir) {
		return
	}
	path := filepath.Join(dir, providerFile)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024 {
		return
	}
	handle, err := lock.TryAcquireExisting(path)
	if err != nil {
		return
	}
	defer func() { _ = handle.Release() }()
	file := handle.File()
	data, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil || len(data) > 1024 {
		return
	}
	counts := map[string]int64{}
	if json.Unmarshal(data, &counts) != nil || len(counts) > 3 {
		return
	}
	if counts[id] >= 1000000 {
		return
	}
	counts[id]++
	data, err = json.Marshal(counts)
	if err != nil || len(data) > 1024 {
		return
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return
	}
	if err = file.Truncate(0); err != nil {
		return
	}
	_, _ = file.Write(data)
}

// CollectProviderWindow persists observed HTTP attempts through the same
// recorder used by local and remote executors. Coverage remains partial:
// arbitrary CLIs and daemon polling outside this stage are not measured, and
// interrupted or contended sidecar writes may be absent. Zero is never emitted.
func CollectProviderWindow(dir string, recorder any, stage string) {
	appender, ok := recorder.(Appender)
	if !ok || !plainDirectory(dir) {
		return
	}
	budget := int64(1024)
	data, err := boundedFile(filepath.Join(dir, providerFile), 1024, &budget)
	if err != nil {
		return
	}
	counts := map[string]int64{}
	if json.Unmarshal(data, &counts) != nil || len(counts) > 3 {
		return
	}
	for _, id := range []string{"provider.github", "provider.gitea", "provider.azure-devops"} {
		count := counts[id]
		if count <= 0 || count > 1000000 {
			continue
		}
		_ = appender.Append(journal.Event{Type: journal.EventRunnerAnnotation, Time: time.Now().UTC(), Stage: stage, Runner: map[string]any{"kind": Kind, "featureId": id, "count": count}})
	}
}
