// Package selfupdate stages and supervises binary-only Goobers updates.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
)

const (
	// PolicyManual stages an explicitly selected release.
	PolicyManual = "manual"
	// PolicyOnRelease tracks the latest tagged release.
	PolicyOnRelease = "on-release"
	// PolicyOnMain tracks the configured product repository branch.
	PolicyOnMain = "on-main"

	requestSchema = "goobers.dev/self-update/v1"
	// DefaultHealthTicks is the required number of clean heartbeat intervals.
	DefaultHealthTicks = 3
	// DefaultHealthTimeout bounds candidate health monitoring.
	DefaultHealthTimeout = 5 * time.Minute
	defaultPollInterval  = time.Second
	maxArchiveBytes      = 1 << 30
	maxMetadataBytes     = 1 << 20
)

// Request is the durable workflow-to-supervisor handoff.
type Request struct {
	Schema        string    `json:"schema"`
	RunID         string    `json:"runId,omitempty"`
	Policy        string    `json:"policy"`
	Owner         string    `json:"owner"`
	Repository    string    `json:"repository"`
	Target        string    `json:"target"`
	StagedPath    string    `json:"stagedPath"`
	RequestedAt   time.Time `json:"requestedAt"`
	HealthTicks   int       `json:"healthTicks"`
	HealthTimeout string    `json:"healthTimeout"`
	Status        string    `json:"status"`
	RollbackReady bool      `json:"rollbackReady,omitempty"`
	Reason        string    `json:"reason,omitempty"`
}

// PrepareOptions configures update discovery, staging, and validation.
type PrepareOptions struct {
	Root, WorkDir, Policy, Owner, Repository, Branch, Target, Token, RunID string
	HealthTicks                                                            int
	HealthTimeout, HeartbeatInterval                                       time.Duration
	GOOS, GOARCH, APIBaseURL                                               string
	HTTPClient                                                             *http.Client
	Runner                                                                 commandRunner
}

// PrepareResult describes the target selected by an update check.
type PrepareResult struct {
	UpdateRequested bool   `json:"updateRequested"`
	Policy          string `json:"policy"`
	Target          string `json:"target,omitempty"`
}

type commandRunner interface {
	Run(context.Context, string, []string, string, ...string) ([]byte, error)
}
type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

type versionInfo struct{ Version, Commit string }
type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Prepare discovers, stages, validates, and publishes an update request.
func Prepare(ctx context.Context, opts PrepareOptions) (_ PrepareResult, retErr error) {
	opts = defaultPrepareOptions(opts)
	if err := validatePrepareOptions(opts); err != nil {
		return PrepareResult{}, err
	}
	if err := os.MkdirAll(updatesDir(opts.Root), 0o755); err != nil {
		return PrepareResult{}, fmt.Errorf("create self-update directory: %w", err)
	}
	currentPath := currentBinary(opts.Root, opts.GOOS)
	if _, err := os.Stat(currentPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PrepareResult{}, errors.New("self-update requires `goobers service install` and a running supervised daemon")
		}
		return PrepareResult{}, fmt.Errorf("inspect supervised binary: %w", err)
	}
	if _, err := os.Stat(requestPath(opts.Root)); err == nil {
		return PrepareResult{}, errors.New("a self-update handoff is already pending")
	} else if !errors.Is(err, os.ErrNotExist) {
		return PrepareResult{}, fmt.Errorf("inspect self-update request: %w", err)
	}
	current, err := readVersion(ctx, opts.Runner, opts.WorkDir, currentPath)
	if err != nil {
		return PrepareResult{}, fmt.Errorf("read active binary version: %w", err)
	}

	var target, version, commit, staged string
	if opts.Policy == PolicyOnMain {
		commit, err = resolveMainCommit(ctx, opts)
		target = opts.Branch + "@" + commit
		if err == nil && !commitsEqual(current.Commit, commit) {
			staged, err = stageMain(ctx, opts, commit)
		}
	} else {
		var release githubRelease
		release, err = resolveRelease(ctx, opts)
		target, version = release.TagName, release.TagName
		if err == nil && current.Version != version {
			staged, err = stageRelease(ctx, opts, release)
		}
	}
	if err != nil {
		return PrepareResult{}, err
	}
	if staged == "" {
		return PrepareResult{Policy: opts.Policy, Target: target}, nil
	}
	published := false
	defer func() {
		if !published {
			retErr = errors.Join(retErr, removeCompletedStaging(opts.Root, staged))
		}
	}()
	info, err := smokeCheck(ctx, opts, staged)
	if err != nil {
		return PrepareResult{}, err
	}
	if version != "" && info.Version != version {
		return PrepareResult{}, fmt.Errorf("staged binary reports version %q, want %q", info.Version, version)
	}
	if commit != "" && !commitsEqual(info.Commit, commit) {
		return PrepareResult{}, fmt.Errorf("staged binary reports commit %q, want %q", info.Commit, commit)
	}
	request := Request{
		RunID: opts.RunID, Policy: opts.Policy, Owner: opts.Owner, Repository: opts.Repository,
		Target: target, StagedPath: staged,
		RequestedAt: time.Now().UTC(), HealthTicks: opts.HealthTicks,
		HealthTimeout: opts.HealthTimeout.String(),
		Status:        "requested",
	}
	published, err = publishRequest(opts.Root, request)
	if err != nil {
		return PrepareResult{}, err
	}
	return PrepareResult{UpdateRequested: true, Policy: opts.Policy, Target: target}, nil
}

func defaultPrepareOptions(opts PrepareOptions) PrepareOptions {
	opts.Policy = valueOr(opts.Policy, PolicyOnRelease)
	opts.Branch = valueOr(opts.Branch, "main")
	opts.HealthTicks = valueOr(opts.HealthTicks, DefaultHealthTicks)
	opts.HealthTimeout = valueOr(opts.HealthTimeout, DefaultHealthTimeout)
	opts.HeartbeatInterval = valueOr(opts.HeartbeatInterval, time.Minute)
	opts.GOOS = valueOr(opts.GOOS, runtime.GOOS)
	opts.GOARCH = valueOr(opts.GOARCH, runtime.GOARCH)
	opts.APIBaseURL = valueOr(opts.APIBaseURL, "https://api.github.com")
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 2 * time.Minute}
	}
	if opts.Runner == nil {
		opts.Runner = execRunner{}
	}
	return opts
}

func valueOr[T comparable](value, fallback T) T {
	var zero T
	if value == zero {
		return fallback
	}
	return value
}

func validatePrepareOptions(opts PrepareOptions) error {
	if opts.Root == "" || opts.WorkDir == "" || opts.Owner == "" || opts.Repository == "" {
		return errors.New("instance root, working directory, and product repository are required")
	}
	switch opts.Policy {
	case PolicyOnRelease:
		if opts.Target != "" {
			return errors.New("on-release policy does not accept an explicit target")
		}
	case PolicyManual:
		if opts.Target == "" {
			return errors.New("manual policy requires an explicit release tag target")
		}
	case PolicyOnMain:
		if opts.Target != "" {
			return errors.New("on-main policy tracks the configured branch and does not accept target")
		}
	default:
		return fmt.Errorf("unknown self-update policy %q (want manual, on-release, or on-main)", opts.Policy)
	}
	minimum := time.Duration(opts.HealthTicks+1) * opts.HeartbeatInterval
	if opts.HealthTicks < 1 || opts.HealthTimeout <= 0 || opts.HeartbeatInterval <= 0 || opts.HealthTimeout < minimum {
		return fmt.Errorf("health window requires positive values and at least %s", minimum)
	}
	return nil
}

func resolveRelease(ctx context.Context, opts PrepareOptions) (githubRelease, error) {
	suffix := "/releases/latest"
	if opts.Policy == PolicyManual {
		suffix = "/releases/tags/" + url.PathEscape(opts.Target)
	}
	var release githubRelease
	if err := githubJSON(ctx, opts, suffix, &release); err != nil {
		return release, fmt.Errorf("query GitHub release: %w", err)
	}
	if release.TagName == "" || opts.Policy == PolicyManual && release.TagName != opts.Target {
		return release, fmt.Errorf("GitHub release returned unexpected tag %q", release.TagName)
	}
	return release, nil
}

func resolveMainCommit(ctx context.Context, opts PrepareOptions) (string, error) {
	var response struct {
		SHA string `json:"sha"`
	}
	if err := githubJSON(ctx, opts, "/commits/"+url.PathEscape(opts.Branch), &response); err != nil {
		return "", fmt.Errorf("query configured branch: %w", err)
	}
	response.SHA = strings.TrimSpace(response.SHA)
	if len(response.SHA) != 40 {
		return "", fmt.Errorf("configured branch returned unexpected commit %q", response.SHA)
	}
	if _, err := hex.DecodeString(response.SHA); err != nil {
		return "", fmt.Errorf("configured branch returned unexpected commit %q", response.SHA)
	}
	return response.SHA, nil
}

func githubJSON(ctx context.Context, opts PrepareOptions, suffix string, target any) error {
	response, err := get(ctx, opts, repositoryEndpoint(opts)+suffix)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	return json.NewDecoder(io.LimitReader(response.Body, maxMetadataBytes)).Decode(target)
}

func stageRelease(ctx context.Context, opts PrepareOptions, release githubRelease) (_ string, retErr error) {
	extension := "tar.gz"
	if opts.GOOS == "windows" {
		extension = "zip"
	}
	archiveName := fmt.Sprintf("goobers_%s_%s_%s.%s", release.TagName, opts.GOOS, opts.GOARCH, extension)
	var archiveURL, sumsURL string
	for _, asset := range release.Assets {
		switch asset.Name {
		case archiveName:
			archiveURL = asset.URL
		case "SHA256SUMS":
			sumsURL = asset.URL
		}
	}
	if archiveURL == "" || sumsURL == "" {
		return "", fmt.Errorf("release %s is missing %s or SHA256SUMS", release.TagName, archiveName)
	}
	dir, cleanup, err := newStageDir(opts.Root, release.TagName)
	if err != nil {
		return "", err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, cleanup())
		}
	}()
	archivePath, sumsPath := filepath.Join(dir, archiveName), filepath.Join(dir, "SHA256SUMS")
	if err := download(ctx, opts, archiveURL, archivePath, maxArchiveBytes); err != nil {
		return "", fmt.Errorf("download %s: %w", archiveName, err)
	}
	if err := download(ctx, opts, sumsURL, sumsPath, maxMetadataBytes); err != nil {
		return "", fmt.Errorf("download SHA256SUMS: %w", err)
	}
	if err := verifyChecksum(archivePath, sumsPath, archiveName); err != nil {
		return "", err
	}
	binary := filepath.Join(dir, binaryName(opts.GOOS))
	if opts.GOOS == "windows" {
		err = extractZipBinary(archivePath, binary)
	} else {
		err = extractTarBinary(archivePath, binary)
	}
	if err != nil {
		return "", err
	}
	if err := errors.Join(os.Remove(archivePath), os.Remove(sumsPath)); err != nil {
		return "", fmt.Errorf("remove downloaded release metadata: %w", err)
	}
	return binary, nil
}

func stageMain(ctx context.Context, opts PrepareOptions, commit string) (_ string, retErr error) {
	dir, cleanup, err := newStageDir(opts.Root, commit)
	if err != nil {
		return "", err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, cleanup())
		}
	}()
	source := filepath.Join(dir, "source")
	askpass, err := credentials.WriteAskpassScript(dir)
	if err != nil {
		return "", fmt.Errorf("prepare git authentication: %w", err)
	}
	cloneURL := fmt.Sprintf("https://github.com/%s/%s.git", url.PathEscape(opts.Owner), url.PathEscape(opts.Repository))
	if _, err := opts.Runner.Run(ctx, dir, credentials.GitAuthEnvironment(askpass, opts.Token), "git", "clone", "--quiet", "--no-hardlinks", "--no-checkout", cloneURL, source); err != nil {
		return "", fmt.Errorf("clone configured repository: %w", err)
	}
	if _, err := opts.Runner.Run(ctx, source, nil, "git", "checkout", "--quiet", "--detach", commit); err != nil {
		return "", fmt.Errorf("check out configured commit %s: %w", commit, err)
	}
	binary := filepath.Join(dir, binaryName(opts.GOOS))
	const versionPackage = "github.com/goobers/goobers/internal/version"
	ldflags := fmt.Sprintf("-s -w -X %s.Version=dev -X %s.Commit=%s -X %s.Date=%s",
		versionPackage, versionPackage, commit, versionPackage, time.Now().UTC().Format(time.RFC3339))
	if _, err := opts.Runner.Run(ctx, source, nil, "go", "build", "-trimpath", "-ldflags", ldflags, "-o", binary, "./cmd/goobers"); err != nil {
		return "", fmt.Errorf("build main target %s: %w", commit, err)
	}
	if err := os.RemoveAll(source); err != nil {
		return "", fmt.Errorf("clean staged source: %w", err)
	}
	return binary, nil
}

func newStageDir(root, target string) (string, func() error, error) {
	// Guard the staging root itself before touching the digest subdirectory:
	// if something occupies stagingDir(root) with a non-directory (a stray
	// file left by a prior crash, corruption, etc.), os.RemoveAll below can't
	// be trusted to surface that reliably across platforms. On Windows,
	// looking up a path whose parent component is a plain file returns
	// ERROR_PATH_NOT_FOUND, which os.IsNotExist treats as "already gone" —
	// so RemoveAll(dir) silently no-ops instead of reporting the collision,
	// and the failure only resurfaces later from MkdirAll with an unrelated,
	// confusing error. Detect the collision explicitly up front and fail
	// closed instead of guessing whether it's safe to delete.
	staging := stagingDir(root)
	if info, err := os.Lstat(staging); err != nil {
		if !os.IsNotExist(err) {
			return "", nil, fmt.Errorf("clear staging directory: %w", err)
		}
	} else if !info.IsDir() {
		return "", nil, fmt.Errorf("clear staging directory: %s is not a directory", staging)
	}

	dir := filepath.Join(staging, digestName(target))
	if err := os.RemoveAll(dir); err != nil {
		return "", nil, fmt.Errorf("clear staging directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create staging directory: %w", err)
	}
	return dir, func() error { return os.RemoveAll(dir) }, nil
}

func smokeCheck(ctx context.Context, opts PrepareOptions, binary string) (versionInfo, error) {
	info, err := readVersion(ctx, opts.Runner, opts.WorkDir, binary)
	if err != nil {
		return info, fmt.Errorf("staged --version smoke check: %w", err)
	}
	if _, err := opts.Runner.Run(ctx, opts.WorkDir, nil, binary, "validate", opts.Root); err != nil {
		return info, fmt.Errorf("staged validate smoke check: %w", err)
	}
	canonical := opts.Root
	candidate := filepath.Join(opts.WorkDir, "reference-workflows")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		canonical = candidate
	}
	if _, err := opts.Runner.Run(ctx, opts.WorkDir, nil, binary, "config", "diff", "--against", canonical, opts.Root); err != nil {
		return info, fmt.Errorf("staged config diff smoke check: %w", err)
	}
	return info, nil
}

func readVersion(ctx context.Context, runner commandRunner, dir, binary string) (versionInfo, error) {
	raw, err := runner.Run(ctx, dir, nil, binary, "version", "--json")
	if err != nil {
		return versionInfo{}, err
	}
	var info versionInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return info, fmt.Errorf("decode version output: %w", err)
	}
	if info.Version == "" || info.Commit == "" {
		return info, errors.New("version output is missing version or commit")
	}
	return info, nil
}

func download(ctx context.Context, opts PrepareOptions, endpoint, destination string, limit int64) (retErr error) {
	response, err := get(ctx, opts, endpoint)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, response.Body.Close()) }()
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, limit+1))
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return err
	}
	if written > limit {
		return fmt.Errorf("asset exceeds %d bytes", limit)
	}
	return nil
}

func get(ctx context.Context, opts PrepareOptions, endpoint string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if opts.Token != "" {
		request.Header.Set("Authorization", "Bearer "+opts.Token)
	}
	response, err := opts.HTTPClient.Do(request)
	if err == nil && response.StatusCode != http.StatusOK {
		err = fmt.Errorf("%s returned status %d", endpoint, response.StatusCode)
		_ = response.Body.Close()
	}
	return response, err
}

func verifyChecksum(archivePath, sumsPath, archiveName string) error {
	raw, err := os.ReadFile(sumsPath)
	if err != nil {
		return fmt.Errorf("read SHA256SUMS: %w", err)
	}
	var expected string
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == archiveName {
			expected = fields[0]
		}
	}
	archive, err := os.ReadFile(archivePath)
	if err != nil {
		return err
	}
	actualSum := sha256.Sum256(archive)
	actual := hex.EncodeToString(actualSum[:])
	if len(expected) != sha256.Size*2 || !strings.EqualFold(actual, expected) {
		return fmt.Errorf("checksum mismatch for %s", archiveName)
	}
	return nil
}

func extractTarBinary(archivePath, destination string) error {
	found := false
	err := readTarGz(archivePath, func(header *tar.Header, reader *tar.Reader) (bool, error) {
		if filepath.ToSlash(header.Name) != "goobers" || !header.FileInfo().Mode().IsRegular() {
			return false, nil
		}
		found = true
		return true, writeExecutable(destination, io.LimitReader(reader, maxArchiveBytes))
	})
	if err == nil && !found {
		return errors.New("release archive does not contain goobers")
	}
	return err
}
func extractZipBinary(archivePath, destination string) (retErr error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open release zip: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	source, err := reader.Open("goobers.exe")
	if err != nil {
		return errors.New("release archive does not contain goobers.exe")
	}
	err = writeExecutable(destination, io.LimitReader(source, maxArchiveBytes))
	return errors.Join(err, source.Close())
}
func readTarGz(archivePath string, visit func(*tar.Header, *tar.Reader) (bool, error)) (retErr error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, gzipReader.Close()) }()
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		done, err := visit(header, reader)
		if err != nil || done {
			return err
		}
	}
}
func writeExecutable(destination string, source io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	raw, err := io.ReadAll(source)
	if err != nil {
		return err
	}
	return journal.WriteFileAtomic(destination, raw, 0o755)
}
func commitsEqual(left, right string) bool {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	return left != "" && right != "" && left != "none" && right != "none" &&
		(strings.HasPrefix(left, right) || strings.HasPrefix(right, left))
}
func digestName(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}
func repositoryEndpoint(opts PrepareOptions) string {
	return strings.TrimRight(opts.APIBaseURL, "/") + "/repos/" + url.PathEscape(opts.Owner) + "/" + url.PathEscape(opts.Repository)
}
func binaryName(goos string) string {
	if goos == "windows" {
		return "goobers.exe"
	}
	return "goobers"
}
func updatesDir(root string) string      { return filepath.Join(root, "updates") }
func stagingDir(root string) string      { return filepath.Join(updatesDir(root), "staged") }
func requestPath(root string) string     { return filepath.Join(updatesDir(root), "request.json") }
func stopRequestPath(root string) string { return filepath.Join(updatesDir(root), "stop-request") }
func currentBinary(root, goos string) string {
	return filepath.Join(updatesDir(root), "current", binaryName(goos))
}
func previousBinary(root, goos string) string {
	return filepath.Join(updatesDir(root), "previous", binaryName(goos))
}
func writeRequest(root string, request Request) error {
	_, err := publishRequest(root, request)
	return err
}
func publishRequest(root string, request Request) (bool, error) {
	if request.Schema == "" {
		request.Schema = requestSchema
	}
	if request.Schema != requestSchema {
		return false, fmt.Errorf("unsupported self-update request schema %q", request.Schema)
	}
	return writeJSONAtomic(requestPath(root), request)
}
func readRequest(root string) (Request, error) {
	raw, err := os.ReadFile(requestPath(root))
	if err != nil {
		return Request{}, err
	}
	var request Request
	if err := json.Unmarshal(raw, &request); err != nil {
		return request, fmt.Errorf("decode self-update request: %w", err)
	}
	if request.Schema != requestSchema {
		return request, fmt.Errorf("unsupported self-update request schema %q", request.Schema)
	}
	return request, nil
}
func writeJSONAtomic(path string, value any) (bool, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode self-update state: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	err = journal.WriteFileAtomic(path, raw, 0o600)
	if err != nil {
		_, statErr := os.Stat(path)
		published := statErr == nil
		return published, fmt.Errorf("publish self-update state: %w", err)
	}
	return true, nil
}
