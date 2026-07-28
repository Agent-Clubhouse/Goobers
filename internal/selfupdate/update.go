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
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

const (
	PolicyManual    = "manual"
	PolicyOnRelease = "on-release"
	PolicyOnMain    = "on-main"

	requestSchema = "goobers.dev/self-update/v1"

	DefaultHealthTicks   = 3
	DefaultHealthTimeout = 5 * time.Minute
	DefaultPollInterval  = time.Second

	maxArchiveBytes  = 1 << 30
	maxChecksumsSize = 1 << 20

	prepareLockFile = "prepare.lock"
)

// Request is the durable handoff from the self-update workflow to the stable
// service supervisor.
type Request struct {
	Schema            string    `json:"schema"`
	RunID             string    `json:"runId,omitempty"`
	Policy            string    `json:"policy"`
	Owner             string    `json:"owner"`
	Repository        string    `json:"repository"`
	Target            string    `json:"target"`
	Version           string    `json:"version,omitempty"`
	Commit            string    `json:"commit,omitempty"`
	StagedPath        string    `json:"stagedPath"`
	RequestedAt       time.Time `json:"requestedAt"`
	HealthTicks       int       `json:"healthTicks"`
	HealthTimeout     string    `json:"healthTimeout"`
	HeartbeatInterval string    `json:"heartbeatInterval"`
	Status            string    `json:"status"`
	Reason            string    `json:"reason,omitempty"`
}

// PrepareOptions configures one deterministic target-detection and staging pass.
type PrepareOptions struct {
	Root              string
	WorkDir           string
	Policy            string
	Owner             string
	Repository        string
	Branch            string
	Target            string
	Token             string
	RunID             string
	HealthTicks       int
	HealthTimeout     time.Duration
	HeartbeatInterval time.Duration
	GOOS              string
	GOARCH            string
	APIBaseURL        string
	HTTPClient        *http.Client
	Runner            CommandRunner
	Now               func() time.Time
}

// PrepareResult reports whether a new target was handed to the supervisor.
type PrepareResult struct {
	UpdateRequested bool   `json:"updateRequested"`
	Policy          string `json:"policy"`
	Target          string `json:"target,omitempty"`
	StagedPath      string `json:"stagedPath,omitempty"`
}

// CommandRunner runs build and smoke-check commands.
type CommandRunner interface {
	Run(context.Context, string, string, ...string) ([]byte, error)
}

// ExecRunner runs commands as local child processes.
type ExecRunner struct{}

// Run executes name in dir and returns its combined output.
func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			return output, fmt.Errorf("%s: %w", name, err)
		}
		return output, fmt.Errorf("%s: %w: %s", name, err, detail)
	}
	return output, nil
}

type versionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

type githubCommit struct {
	SHA string `json:"sha"`
}

// Prepare detects the configured target, stages and validates it, then writes a
// durable request. It never changes the active binary or configuration tree.
func Prepare(ctx context.Context, opts PrepareOptions) (_ PrepareResult, retErr error) {
	opts = defaultPrepareOptions(opts)
	if err := validatePrepareOptions(opts); err != nil {
		return PrepareResult{}, err
	}
	if err := os.MkdirAll(UpdatesDir(opts.Root), 0o755); err != nil {
		return PrepareResult{}, fmt.Errorf("create self-update directory: %w", err)
	}
	held, err := platformlock.TryAcquire(filepath.Join(UpdatesDir(opts.Root), prepareLockFile))
	if errors.Is(err, platformlock.ErrHeld) {
		return PrepareResult{}, errors.New("another self-update preparation is already in progress")
	}
	if err != nil {
		return PrepareResult{}, fmt.Errorf("reserve self-update preparation: %w", err)
	}
	defer func() {
		if err := held.Release(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release self-update preparation: %w", err))
		}
	}()
	if _, err := os.Stat(CurrentBinary(opts.Root, opts.GOOS)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PrepareResult{}, errors.New("self-update requires `goobers service install` and a running supervised daemon")
		}
		return PrepareResult{}, fmt.Errorf("inspect supervised binary: %w", err)
	}
	if _, err := os.Stat(RequestPath(opts.Root)); err == nil {
		return PrepareResult{}, errors.New("a self-update handoff is already pending")
	} else if !errors.Is(err, os.ErrNotExist) {
		return PrepareResult{}, fmt.Errorf("inspect self-update request: %w", err)
	}

	current, err := readVersion(ctx, opts.Runner, opts.WorkDir, CurrentBinary(opts.Root, opts.GOOS))
	if err != nil {
		return PrepareResult{}, fmt.Errorf("read active binary version: %w", err)
	}

	var target, version, commit, staged string
	stagingPublished := false
	defer func() {
		if staged == "" || stagingPublished {
			return
		}
		if err := removeCompletedStaging(opts.Root, staged); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("clean up failed self-update staging: %w", err))
		}
	}()
	switch opts.Policy {
	case PolicyOnRelease, PolicyManual:
		release, err := resolveRelease(ctx, opts)
		if err != nil {
			return PrepareResult{}, err
		}
		target, version = release.TagName, release.TagName
		if current.Version == version {
			return PrepareResult{Policy: opts.Policy, Target: target}, nil
		}
		staged, err = stageRelease(ctx, opts, release)
		if err != nil {
			return PrepareResult{}, err
		}
	case PolicyOnMain:
		commit, err = resolveMainCommit(ctx, opts)
		if err != nil {
			return PrepareResult{}, err
		}
		target = opts.Branch + "@" + commit
		if commitsEqual(current.Commit, commit) {
			return PrepareResult{Policy: opts.Policy, Target: target}, nil
		}
		staged, err = stageMain(ctx, opts, commit)
		if err != nil {
			return PrepareResult{}, err
		}
	default:
		panic("validated policy")
	}

	stagedVersion, err := smokeCheck(ctx, opts, staged)
	if err != nil {
		return PrepareResult{}, err
	}
	if version != "" && stagedVersion.Version != version {
		return PrepareResult{}, fmt.Errorf("staged binary reports version %q, want %q", stagedVersion.Version, version)
	}
	if commit != "" && !commitsEqual(stagedVersion.Commit, commit) {
		return PrepareResult{}, fmt.Errorf("staged binary reports commit %q, want %q", stagedVersion.Commit, commit)
	}

	request := Request{
		Schema:            requestSchema,
		RunID:             opts.RunID,
		Policy:            opts.Policy,
		Owner:             opts.Owner,
		Repository:        opts.Repository,
		Target:            target,
		Version:           version,
		Commit:            commit,
		StagedPath:        staged,
		RequestedAt:       opts.Now().UTC(),
		HealthTicks:       opts.HealthTicks,
		HealthTimeout:     opts.HealthTimeout.String(),
		HeartbeatInterval: opts.HeartbeatInterval.String(),
		Status:            "requested",
	}
	if err := WriteRequest(opts.Root, request); err != nil {
		return PrepareResult{}, err
	}
	stagingPublished = true
	return PrepareResult{
		UpdateRequested: true,
		Policy:          opts.Policy,
		Target:          target,
		StagedPath:      staged,
	}, nil
}

func defaultPrepareOptions(opts PrepareOptions) PrepareOptions {
	if opts.Policy == "" {
		opts.Policy = PolicyOnRelease
	}
	if opts.Branch == "" {
		opts.Branch = "main"
	}
	if opts.HealthTicks == 0 {
		opts.HealthTicks = DefaultHealthTicks
	}
	if opts.HealthTimeout == 0 {
		opts.HealthTimeout = DefaultHealthTimeout
	}
	if opts.HeartbeatInterval == 0 {
		opts.HeartbeatInterval = time.Minute
	}
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.GOARCH == "" {
		opts.GOARCH = runtime.GOARCH
	}
	if opts.APIBaseURL == "" {
		opts.APIBaseURL = "https://api.github.com"
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 2 * time.Minute}
	}
	if opts.Runner == nil {
		opts.Runner = ExecRunner{}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return opts
}

func validatePrepareOptions(opts PrepareOptions) error {
	if opts.Root == "" || opts.WorkDir == "" {
		return errors.New("instance root and working directory are required")
	}
	if opts.Owner == "" || opts.Repository == "" {
		return errors.New("product repository owner and name are required")
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
		if opts.Branch == "" {
			return errors.New("on-main policy requires a branch")
		}
	default:
		return fmt.Errorf("unknown self-update policy %q (want manual, on-release, or on-main)", opts.Policy)
	}
	if opts.HealthTicks < 1 {
		return errors.New("health ticks must be positive")
	}
	if opts.HealthTimeout <= 0 {
		return errors.New("health timeout must be positive")
	}
	if opts.HeartbeatInterval <= 0 {
		return errors.New("heartbeat interval must be positive")
	}
	minimumWindow := time.Duration(opts.HealthTicks+1) * opts.HeartbeatInterval
	if opts.HealthTimeout < minimumWindow {
		return fmt.Errorf("health timeout must be at least %s for %d heartbeat ticks at %s intervals", minimumWindow, opts.HealthTicks, opts.HeartbeatInterval)
	}
	return nil
}

func resolveRelease(ctx context.Context, opts PrepareOptions) (githubRelease, error) {
	endpoint := strings.TrimRight(opts.APIBaseURL, "/") + "/repos/" +
		url.PathEscape(opts.Owner) + "/" + url.PathEscape(opts.Repository) + "/releases/"
	if opts.Policy == PolicyManual {
		endpoint += "tags/" + url.PathEscape(opts.Target)
	} else {
		endpoint += "latest"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return githubRelease{}, fmt.Errorf("build release request: %w", err)
	}
	setGitHubHeaders(req, opts.Token)
	response, err := opts.HTTPClient.Do(req)
	if err != nil {
		return githubRelease{}, fmt.Errorf("query GitHub release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return githubRelease{}, fmt.Errorf("query GitHub release: status %d: %s", response.StatusCode, strings.TrimSpace(string(detail)))
	}
	var release githubRelease
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&release); err != nil {
		return githubRelease{}, fmt.Errorf("decode GitHub release: %w", err)
	}
	if release.TagName == "" {
		return githubRelease{}, errors.New("GitHub release has no tag")
	}
	if opts.Policy == PolicyManual && release.TagName != opts.Target {
		return githubRelease{}, fmt.Errorf("GitHub release tag %q does not match manual target %q", release.TagName, opts.Target)
	}
	return release, nil
}

func resolveMainCommit(ctx context.Context, opts PrepareOptions) (string, error) {
	endpoint := githubRepositoryEndpoint(opts) + "/commits/" + url.PathEscape(opts.Branch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build main target request: %w", err)
	}
	setGitHubHeaders(req, opts.Token)
	response, err := opts.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("query configured branch: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("query configured branch: status %d: %s", response.StatusCode, strings.TrimSpace(string(detail)))
	}
	var commit githubCommit
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&commit); err != nil {
		return "", fmt.Errorf("decode configured branch: %w", err)
	}
	commit.SHA = strings.TrimSpace(commit.SHA)
	if !validCommit(commit.SHA) {
		return "", fmt.Errorf("configured branch returned unexpected commit %q", commit.SHA)
	}
	return commit.SHA, nil
}

func stageRelease(ctx context.Context, opts PrepareOptions, release githubRelease) (_ string, retErr error) {
	extension := "tar.gz"
	if opts.GOOS == "windows" {
		extension = "zip"
	}
	archiveName := fmt.Sprintf("goobers_%s_%s_%s.%s", release.TagName, opts.GOOS, opts.GOARCH, extension)
	archiveURL, sumsURL := "", ""
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

	stageDir := filepath.Join(StagingDir(opts.Root), digestName(release.TagName))
	if err := os.RemoveAll(stageDir); err != nil {
		return "", fmt.Errorf("clear release staging directory: %w", err)
	}
	complete := false
	defer func() {
		if complete {
			return
		}
		if err := os.RemoveAll(stageDir); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("clean up release staging directory: %w", err))
		}
	}()
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return "", fmt.Errorf("create release staging directory: %w", err)
	}
	archivePath := filepath.Join(stageDir, archiveName)
	if err := downloadAsset(ctx, opts, archiveURL, archivePath, maxArchiveBytes); err != nil {
		return "", fmt.Errorf("download %s: %w", archiveName, err)
	}
	defer os.Remove(archivePath)
	sumsPath := filepath.Join(stageDir, "SHA256SUMS")
	if err := downloadAsset(ctx, opts, sumsURL, sumsPath, maxChecksumsSize); err != nil {
		return "", fmt.Errorf("download SHA256SUMS: %w", err)
	}
	defer os.Remove(sumsPath)
	if err := verifyChecksum(archivePath, sumsPath, archiveName); err != nil {
		return "", err
	}

	binary := filepath.Join(stageDir, binaryName(opts.GOOS))
	if opts.GOOS == "windows" {
		if err := extractZipBinary(archivePath, binary); err != nil {
			return "", err
		}
	} else if err := extractTarBinary(archivePath, binary); err != nil {
		return "", err
	}
	complete = true
	return binary, nil
}

func stageMain(ctx context.Context, opts PrepareOptions, commit string) (_ string, retErr error) {
	stageDir := filepath.Join(StagingDir(opts.Root), digestName(commit))
	if err := os.RemoveAll(stageDir); err != nil {
		return "", fmt.Errorf("clear main staging directory: %w", err)
	}
	complete := false
	defer func() {
		if complete {
			return
		}
		if err := os.RemoveAll(stageDir); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("clean up main staging directory: %w", err))
		}
	}()
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return "", fmt.Errorf("create main staging directory: %w", err)
	}
	archive := filepath.Join(stageDir, "source.tar.gz")
	source := filepath.Join(stageDir, "source")
	if err := downloadAsset(ctx, opts, githubRepositoryEndpoint(opts)+"/tarball/"+url.PathEscape(commit), archive, maxArchiveBytes); err != nil {
		return "", fmt.Errorf("download configured commit %s: %w", commit, err)
	}
	defer os.Remove(archive)
	if err := extractSourceTarGz(archive, source); err != nil {
		return "", err
	}
	defer os.RemoveAll(source)

	binary := filepath.Join(stageDir, binaryName(opts.GOOS))
	if err := os.Remove(binary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("clear staged main binary: %w", err)
	}
	const versionPackage = "github.com/goobers/goobers/internal/version"
	ldflags := strings.Join([]string{
		"-s", "-w",
		"-X", versionPackage + ".Version=dev",
		"-X", versionPackage + ".Commit=" + commit,
		"-X", versionPackage + ".Date=" + opts.Now().UTC().Format(time.RFC3339),
	}, " ")
	if _, err := opts.Runner.Run(ctx, source, "go", "build", "-trimpath", "-ldflags", ldflags, "-o", binary, "./cmd/goobers"); err != nil {
		return "", fmt.Errorf("build main target %s: %w", commit, err)
	}
	complete = true
	return binary, nil
}

func githubRepositoryEndpoint(opts PrepareOptions) string {
	return strings.TrimRight(opts.APIBaseURL, "/") + "/repos/" +
		url.PathEscape(opts.Owner) + "/" + url.PathEscape(opts.Repository)
}

func smokeCheck(ctx context.Context, opts PrepareOptions, binary string) (versionInfo, error) {
	info, err := readVersion(ctx, opts.Runner, opts.WorkDir, binary)
	if err != nil {
		return versionInfo{}, fmt.Errorf("staged --version smoke check: %w", err)
	}
	if _, err := opts.Runner.Run(ctx, opts.WorkDir, binary, "validate", opts.Root); err != nil {
		return versionInfo{}, fmt.Errorf("staged validate smoke check: %w", err)
	}
	canonicalRoot, err := smokeCanonicalRoot(opts)
	if err != nil {
		return versionInfo{}, err
	}
	if _, err := opts.Runner.Run(ctx, opts.WorkDir, binary, "config", "diff", "--against", canonicalRoot, opts.Root); err != nil {
		return versionInfo{}, fmt.Errorf("staged config diff smoke check: %w", err)
	}
	return info, nil
}

func smokeCanonicalRoot(opts PrepareOptions) (string, error) {
	candidate := filepath.Join(opts.WorkDir, "selfhost")
	info, err := os.Stat(candidate)
	if err == nil && info.IsDir() {
		return candidate, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect canonical config source %s: %w", candidate, err)
	}
	return opts.Root, nil
}

func readVersion(ctx context.Context, runner CommandRunner, dir, binary string) (versionInfo, error) {
	output, err := runner.Run(ctx, dir, binary, "version", "--json")
	if err != nil {
		return versionInfo{}, err
	}
	var info versionInfo
	if err := json.Unmarshal(output, &info); err != nil {
		return versionInfo{}, fmt.Errorf("decode version output: %w", err)
	}
	if info.Version == "" || info.Commit == "" {
		return versionInfo{}, errors.New("version output is missing version or commit")
	}
	return info, nil
}

func downloadAsset(ctx context.Context, opts PrepareOptions, assetURL, path string, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return err
	}
	setGitHubHeaders(req, opts.Token)
	response, err := opts.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", response.StatusCode)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, limit+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > limit {
		return fmt.Errorf("asset exceeds %d bytes", limit)
	}
	return nil
}

func setGitHubHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func verifyChecksum(archivePath, sumsPath, archiveName string) error {
	raw, err := os.ReadFile(sumsPath)
	if err != nil {
		return fmt.Errorf("read SHA256SUMS: %w", err)
	}
	expected := ""
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == archiveName {
			expected = fields[0]
			break
		}
	}
	if len(expected) != sha256.Size*2 {
		return fmt.Errorf("SHA256SUMS does not contain a valid digest for %s", archiveName)
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open staged archive: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("hash staged archive: %w", err)
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); !strings.EqualFold(actual, expected) {
		return fmt.Errorf("checksum mismatch for %s", archiveName)
	}
	return nil
}

func extractTarBinary(archivePath, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open release archive: %w", err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open release gzip stream: %w", err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read release archive: %w", err)
		}
		if filepath.ToSlash(header.Name) != "goobers" || !header.FileInfo().Mode().IsRegular() {
			continue
		}
		return writeExecutable(destination, io.LimitReader(reader, maxArchiveBytes))
	}
	return errors.New("release archive does not contain goobers")
}

func extractZipBinary(archivePath, destination string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open release zip: %w", err)
	}
	defer reader.Close()
	for _, entry := range reader.File {
		if filepath.ToSlash(entry.Name) != "goobers.exe" || !entry.Mode().IsRegular() {
			continue
		}
		source, err := entry.Open()
		if err != nil {
			return fmt.Errorf("open goobers.exe in release zip: %w", err)
		}
		writeErr := writeExecutable(destination, io.LimitReader(source, maxArchiveBytes))
		closeErr := source.Close()
		return errors.Join(writeErr, closeErr)
	}
	return errors.New("release archive does not contain goobers.exe")
}

func extractSourceTarGz(archivePath, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open source archive: %w", err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open source gzip stream: %w", err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	root := ""
	var extracted int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read source archive: %w", err)
		}
		if strings.Contains(header.Name, `\`) {
			return fmt.Errorf("source archive contains invalid path %q", header.Name)
		}
		clean := path.Clean(strings.TrimPrefix(header.Name, "./"))
		if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("source archive contains invalid path %q", header.Name)
		}
		parts := strings.Split(clean, "/")
		if root == "" {
			root = parts[0]
		}
		if parts[0] != root {
			return errors.New("source archive contains multiple roots")
		}
		if len(parts) == 1 {
			if header.Typeflag != tar.TypeDir {
				return errors.New("source archive root is not a directory")
			}
			continue
		}
		relative := path.Join(parts[1:]...)
		target := filepath.Join(destination, filepath.FromSlash(relative))
		contained, err := filepath.Rel(destination, target)
		if err != nil || contained == ".." || strings.HasPrefix(contained, ".."+string(filepath.Separator)) || filepath.IsAbs(contained) {
			return fmt.Errorf("source archive contains invalid path %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create source directory: %w", err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || extracted > maxArchiveBytes-header.Size {
				return fmt.Errorf("expanded source archive exceeds %d bytes", maxArchiveBytes)
			}
			extracted += header.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create source parent directory: %w", err)
			}
			mode := os.FileMode(0o644)
			if header.FileInfo().Mode()&0o111 != 0 {
				mode = 0o755
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return fmt.Errorf("create source file: %w", err)
			}
			_, copyErr := io.CopyN(output, reader, header.Size)
			closeErr := output.Close()
			if copyErr != nil {
				return fmt.Errorf("extract source file: %w", copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close source file: %w", closeErr)
			}
		default:
			return fmt.Errorf("source archive contains unsupported entry %q", header.Name)
		}
	}
	if root == "" {
		return errors.New("source archive is empty")
	}
	return nil
}

func writeExecutable(path string, source io.Reader) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("create staged binary: %w", err)
	}
	if _, err := io.Copy(file, source); err != nil {
		_ = file.Close()
		return fmt.Errorf("write staged binary: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync staged binary: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close staged binary: %w", err)
	}
	return nil
}

func commitsEqual(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" || left == "none" || right == "none" {
		return false
	}
	return strings.HasPrefix(left, right) || strings.HasPrefix(right, left)
}

func validCommit(commit string) bool {
	if len(commit) != 40 {
		return false
	}
	_, err := hex.DecodeString(commit)
	return err == nil
}

func digestName(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func binaryName(goos string) string {
	if goos == "windows" {
		return "goobers.exe"
	}
	return "goobers"
}

// UpdatesDir is the well-known binary maintenance root.
func UpdatesDir(root string) string { return filepath.Join(root, "updates") }

// CurrentBinary is the mutable binary launched by the stable supervisor.
func CurrentBinary(root, goos string) string {
	return filepath.Join(UpdatesDir(root), "current", binaryName(goos))
}

// PreviousBinary is the retained rollback binary.
func PreviousBinary(root, goos string) string {
	return filepath.Join(UpdatesDir(root), "previous", binaryName(goos))
}

// StagingDir contains validated, not-yet-active binaries.
func StagingDir(root string) string { return filepath.Join(UpdatesDir(root), "staged") }

// RequestPath is the durable workflow-to-supervisor handoff.
func RequestPath(root string) string { return filepath.Join(UpdatesDir(root), "request.json") }

// StopRequestPath is the cross-platform graceful-stop request consumed by the daemon.
func StopRequestPath(root string) string { return filepath.Join(UpdatesDir(root), "stop-request") }

// WriteRequest atomically persists a supervisor handoff.
func WriteRequest(root string, request Request) error {
	if request.Schema == "" {
		request.Schema = requestSchema
	}
	if request.Schema != requestSchema {
		return fmt.Errorf("unsupported self-update request schema %q", request.Schema)
	}
	return writeJSONAtomic(RequestPath(root), request)
}

// ReadRequest loads and validates a supervisor handoff.
func ReadRequest(root string) (Request, error) {
	raw, err := os.ReadFile(RequestPath(root))
	if err != nil {
		return Request{}, err
	}
	var request Request
	if err := json.Unmarshal(raw, &request); err != nil {
		return Request{}, fmt.Errorf("decode self-update request: %w", err)
	}
	if request.Schema != requestSchema {
		return Request{}, fmt.Errorf("unsupported self-update request schema %q", request.Schema)
	}
	return request, nil
}

func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create self-update directory: %w", err)
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode self-update state: %w", err)
	}
	raw = append(raw, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return fmt.Errorf("create self-update state: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("secure self-update state: %w", err)
	}
	if _, err := temp.Write(raw); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write self-update state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync self-update state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close self-update state: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish self-update state: %w", err)
	}
	return nil
}
