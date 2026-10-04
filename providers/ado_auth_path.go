package providers

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type azureCLIPathAmbiguityError struct {
	cause          error
	candidateCount int
}

func (e *azureCLIPathAmbiguityError) Error() string { return e.cause.Error() }
func (e *azureCLIPathAmbiguityError) Unwrap() error { return e.cause }

type azureCLIExecRunner struct{}

func (azureCLIExecRunner) Run(ctx context.Context, _ string, args ...string) ([]byte, error) {
	executable, err := exec.LookPath("az")
	if err != nil {
		return nil, err
	}
	if absolute, err := filepath.Abs(executable); err == nil {
		executable = absolute
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if candidates := azureCLIExecutableCandidates(os.Getenv("PATH")); len(candidates) > 1 {
			err = &azureCLIPathAmbiguityError{cause: err, candidateCount: len(candidates)}
		}
	}
	return out, err
}

// azureCLIExecutableCandidates returns at most one executable per PATH entry,
// using the platform's own LookPath rules for extensions and executability.
func azureCLIExecutableCandidates(pathValue string) []string {
	var candidates []string
	seen := make(map[string]struct{})
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" || !filepath.IsAbs(directory) {
			continue
		}
		candidate, err := exec.LookPath(filepath.Join(directory, "az"))
		if err != nil {
			continue
		}
		if absolute, err := filepath.Abs(candidate); err == nil {
			candidate = absolute
		}
		key := filepath.Clean(candidate)
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if _, ok := seen[key]; ok {
			continue
		}
		if sameExecutable(candidate, candidates) {
			continue
		}
		seen[key] = struct{}{}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func sameExecutable(candidate string, existing []string) bool {
	info, err := os.Stat(candidate)
	if err != nil {
		return false
	}
	for _, path := range existing {
		other, err := os.Stat(path)
		if err == nil && os.SameFile(info, other) {
			return true
		}
	}
	return false
}
