// Package portalextension manages the release-matched Goobers Portal canvas
// extension in the current user's Copilot extension directory.
package portalextension

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const (
	// Name is the folder name of the portal canvas extension and the manifest
	// identifier used to install and update it under a Copilot home.
	Name            = "goobers-portal"
	sourceRoot      = ".github/extensions/" + Name
	manifestName    = ".goobers-release.json"
	installRecord   = ".installed-release.sha256"
	pendingRecord   = ".pending-release.sha256"
	manifestSchema  = 1
	stateDirectory  = "extension-state"
	legacyStateDir  = "artifacts"
	defaultFileMode = 0o644
)

// Asset records one file owned by the installed extension.
type Asset struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Manifest ties an installed extension to the exact Goobers binary that
// supplied it.
type Manifest struct {
	SchemaVersion int     `json:"schemaVersion"`
	Name          string  `json:"name"`
	Version       string  `json:"goobersVersion"`
	Commit        string  `json:"goobersCommit"`
	Assets        []Asset `json:"assets"`
}

// Bundle is the immutable extension payload embedded in a Goobers binary.
type Bundle struct {
	Manifest     Manifest
	ManifestJSON []byte
	Files        map[string][]byte
}

// Report describes installation identity and local drift.
type Report struct {
	State            string
	Path             string
	SourceVersion    string
	SourceCommit     string
	InstalledVersion string
	InstalledCommit  string
	Modified         []string
	Missing          []string
	Unexpected       []string
}

// InstallResult reports whether an installation changed.
type InstallResult struct {
	Path      string
	Installed bool
}

// Build creates a release-stamped bundle from the canonical project extension.
func Build(source fs.FS, version, commit string) (Bundle, error) {
	version = strings.TrimSpace(version)
	commit = strings.TrimSpace(commit)
	if version == "" || commit == "" {
		return Bundle{}, fmt.Errorf("portal extension version and commit must not be empty")
	}
	files := map[string][]byte{}
	err := fs.WalkDir(source, sourceRoot, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("portal extension source %s is a symbolic link", name)
		}
		relative := strings.TrimPrefix(strings.TrimPrefix(name, sourceRoot), "/")
		if relative == "" || relative == manifestName || !validRelativePath(relative) {
			return fmt.Errorf("portal extension source path %q is invalid", relative)
		}
		data, err := fs.ReadFile(source, name)
		if err != nil {
			return fmt.Errorf("read portal extension source %s: %w", name, err)
		}
		files[relative] = append([]byte(nil), data...)
		return nil
	})
	if err != nil {
		return Bundle{}, err
	}
	if len(files) == 0 {
		return Bundle{}, fmt.Errorf("portal extension source is empty")
	}
	assets := make([]Asset, 0, len(files))
	for name, data := range files {
		sum := sha256.Sum256(data)
		assets = append(assets, Asset{Path: name, SHA256: fmt.Sprintf("%x", sum)})
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].Path < assets[j].Path })
	manifest := Manifest{
		SchemaVersion: manifestSchema,
		Name:          Name,
		Version:       version,
		Commit:        commit,
		Assets:        assets,
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Bundle{}, fmt.Errorf("encode portal extension manifest: %w", err)
	}
	manifestJSON = append(manifestJSON, '\n')
	return Bundle{Manifest: manifest, ManifestJSON: manifestJSON, Files: files}, nil
}

// DefaultCopilotHome returns the user-scoped Copilot configuration directory.
func DefaultCopilotHome() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("COPILOT_HOME")); configured != "" {
		return filepath.Abs(configured)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".copilot"), nil
}

// Manager manages one user-scoped Portal extension installation.
type Manager struct {
	home string
}

// Open validates a Copilot home and returns its extension manager.
func Open(copilotHome string) (*Manager, error) {
	if strings.TrimSpace(copilotHome) == "" {
		return nil, fmt.Errorf("copilot home must not be empty")
	}
	absolute, err := filepath.Abs(copilotHome)
	if err != nil {
		return nil, fmt.Errorf("resolve Copilot home: %w", err)
	}
	if info, err := os.Lstat(absolute); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("copilot home %s is a symbolic link", absolute)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect copilot home: %w", err)
	}
	return &Manager{home: absolute}, nil
}

// Path returns the user-scoped extension directory.
func (m *Manager) Path() string {
	return filepath.Join(m.home, "extensions", Name)
}

// Check compares the installation with a release-matched bundle.
func (m *Manager) Check(bundle Bundle) (Report, error) {
	report := Report{
		State:         "not-installed",
		Path:          m.Path(),
		SourceVersion: bundle.Manifest.Version,
		SourceCommit:  bundle.Manifest.Commit,
	}
	if err := m.recoverInterruptedReplace(); err != nil {
		return Report{}, err
	}
	info, err := os.Lstat(report.Path)
	if os.IsNotExist(err) {
		return report, nil
	}
	if err != nil {
		return Report{}, fmt.Errorf("inspect portal extension: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Report{}, fmt.Errorf("portal extension target %s is not a safe directory", report.Path)
	}
	manifestData, err := os.ReadFile(filepath.Join(report.Path, manifestName))
	if os.IsNotExist(err) {
		report.State = "unmanaged"
		return report, nil
	}
	if err != nil {
		return Report{}, fmt.Errorf("read installed portal extension manifest: %w", err)
	}
	installed, err := decodeManifest(manifestData)
	if err != nil {
		report.State = "modified"
		report.Modified = []string{manifestName}
		return report, nil
	}
	report.InstalledVersion = installed.Version
	report.InstalledCommit = installed.Commit
	recordMatches, err := m.installRecordMatches(manifestData)
	if err != nil {
		return Report{}, err
	}
	if !recordMatches {
		report.Modified = append(report.Modified, manifestName)
	}
	owned := make(map[string]string, len(installed.Assets))
	for _, asset := range installed.Assets {
		owned[asset.Path] = asset.SHA256
		assetPath := filepath.Join(report.Path, filepath.FromSlash(asset.Path))
		info, statErr := os.Lstat(assetPath)
		if os.IsNotExist(statErr) {
			report.Missing = append(report.Missing, asset.Path)
			continue
		}
		if statErr != nil {
			return Report{}, fmt.Errorf("inspect installed portal extension asset %s: %w", asset.Path, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			report.Modified = append(report.Modified, asset.Path)
			continue
		}
		data, readErr := os.ReadFile(assetPath)
		if readErr != nil {
			return Report{}, fmt.Errorf("read installed portal extension asset %s: %w", asset.Path, readErr)
		}
		sum := sha256.Sum256(data)
		if fmt.Sprintf("%x", sum) != asset.SHA256 {
			report.Modified = append(report.Modified, asset.Path)
		}
	}
	err = filepath.WalkDir(report.Path, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(report.Path, name)
		if err != nil || relative == "." {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if relative == legacyStateDir {
				return filepath.SkipDir
			}
			return nil
		}
		if relative != manifestName {
			if _, ok := owned[relative]; !ok {
				report.Unexpected = append(report.Unexpected, relative)
			}
		}
		return nil
	})
	if err != nil {
		return Report{}, fmt.Errorf("inspect installed portal extension: %w", err)
	}
	sort.Strings(report.Modified)
	sort.Strings(report.Missing)
	sort.Strings(report.Unexpected)
	switch {
	case len(report.Modified)+len(report.Missing)+len(report.Unexpected) > 0:
		report.State = "modified"
	case installed.Version != bundle.Manifest.Version ||
		installed.Commit != bundle.Manifest.Commit ||
		!sameAssets(installed.Assets, bundle.Manifest.Assets):
		report.State = "update-available"
	default:
		report.State = "current"
	}
	return report, nil
}

// Install installs the bundle without claiming an existing unmanaged directory.
func (m *Manager) Install(bundle Bundle) (InstallResult, error) {
	report, err := m.Check(bundle)
	if err != nil {
		return InstallResult{}, err
	}
	switch report.State {
	case "current":
		return InstallResult{Path: report.Path}, nil
	case "not-installed":
		if err := m.replace(bundle, false); err != nil {
			return InstallResult{}, err
		}
		return InstallResult{Path: report.Path, Installed: true}, nil
	case "update-available":
		return InstallResult{}, fmt.Errorf("portal extension %s is already installed; run `goobers portal-extension update`", report.InstalledVersion)
	default:
		return InstallResult{}, fmt.Errorf("portal extension target is %s; refusing to overwrite it", report.State)
	}
}

// Update replaces a managed installation. Modified files require explicit
// acknowledgement.
func (m *Manager) Update(bundle Bundle, replaceModified bool) (InstallResult, error) {
	report, err := m.Check(bundle)
	if err != nil {
		return InstallResult{}, err
	}
	switch report.State {
	case "not-installed":
		return InstallResult{}, fmt.Errorf("Portal extension is not installed; run `goobers portal-extension install`")
	case "unmanaged":
		return InstallResult{}, fmt.Errorf("Portal extension directory is unmanaged; refusing to overwrite it")
	case "modified":
		if !replaceModified {
			return InstallResult{}, fmt.Errorf("Portal extension has local changes; review `goobers portal-extension status` and rerun with --replace-modified")
		}
	case "current":
		return InstallResult{Path: report.Path}, nil
	}
	if err := m.replace(bundle, true); err != nil {
		return InstallResult{}, err
	}
	return InstallResult{Path: report.Path, Installed: true}, nil
}

func (m *Manager) replace(bundle Bundle, existing bool) error {
	extensions := filepath.Join(m.home, "extensions")
	if info, err := os.Lstat(extensions); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("Copilot extensions path %s is not a safe directory", extensions)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect Copilot extensions directory: %w", err)
	}
	if err := os.MkdirAll(extensions, 0o755); err != nil {
		return fmt.Errorf("create Copilot extensions directory: %w", err)
	}
	if existing {
		if err := m.migrateLegacyStateFrom(m.Path()); err != nil {
			return err
		}
	}
	parent := extensions
	stage, err := os.MkdirTemp(parent, "."+Name+"-install-")
	if err != nil {
		return fmt.Errorf("create portal extension staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	for name, data := range bundle.Files {
		target := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create portal extension directory: %w", err)
		}
		if err := os.WriteFile(target, data, defaultFileMode); err != nil {
			return fmt.Errorf("write portal extension asset %s: %w", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(stage, manifestName), bundle.ManifestJSON, defaultFileMode); err != nil {
		return fmt.Errorf("write portal extension manifest: %w", err)
	}
	if !existing {
		if err := os.Rename(stage, m.Path()); err != nil {
			return fmt.Errorf("install portal extension: %w", err)
		}
		if err := m.writeInstallRecord(bundle.ManifestJSON); err != nil {
			_ = os.RemoveAll(m.Path())
			return err
		}
		return nil
	}
	backup := m.Path() + ".previous"
	if err := m.recoverInterruptedReplace(); err != nil {
		return err
	}
	if err := m.writeDigestRecord(m.pendingRecordPath(), bundle.ManifestJSON); err != nil {
		return fmt.Errorf("write pending Portal extension update record: %w", err)
	}
	if err := os.Rename(m.Path(), backup); err != nil {
		_ = os.Remove(m.pendingRecordPath())
		return fmt.Errorf("retain previous portal extension: %w", err)
	}
	if err := os.Rename(stage, m.Path()); err != nil {
		if rollbackErr := os.Rename(backup, m.Path()); rollbackErr != nil {
			return fmt.Errorf("activate portal extension: %w (rollback failed: %w)", err, rollbackErr)
		}
		_ = os.Remove(m.pendingRecordPath())
		return fmt.Errorf("activate portal extension: %w", err)
	}
	if err := m.migrateLegacyStateFrom(backup); err != nil {
		return err
	}
	if err := m.migrateLegacyStateFrom(m.Path()); err != nil {
		return err
	}
	if err := m.writeInstallRecord(bundle.ManifestJSON); err != nil {
		return err
	}
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("remove previous portal extension: %w", err)
	}
	if err := os.Remove(m.pendingRecordPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear pending Portal extension update record: %w", err)
	}
	return nil
}

func (m *Manager) recoverInterruptedReplace() error {
	target, backup := m.Path(), m.Path()+".previous"
	backupInfo, backupErr := os.Lstat(backup)
	if os.IsNotExist(backupErr) {
		return nil
	}
	if backupErr != nil {
		return fmt.Errorf("inspect previous portal extension: %w", backupErr)
	}
	if backupInfo.Mode()&os.ModeSymlink != 0 || !backupInfo.IsDir() {
		return fmt.Errorf("previous portal extension %s is not a safe directory", backup)
	}
	targetInfo, targetErr := os.Lstat(target)
	if os.IsNotExist(targetErr) {
		if err := os.Rename(backup, target); err != nil {
			return fmt.Errorf("restore interrupted portal extension update: %w", err)
		}
		if err := os.Remove(m.pendingRecordPath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clear interrupted Portal extension update record: %w", err)
		}
		return nil
	}
	if targetErr != nil {
		return fmt.Errorf("inspect portal extension during update recovery: %w", targetErr)
	}
	if targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.IsDir() {
		return fmt.Errorf("portal extension target %s is not a safe directory", target)
	}
	manifestData, err := os.ReadFile(filepath.Join(target, manifestName))
	if err != nil {
		return fmt.Errorf("verify activated portal extension during update recovery: %w", err)
	}
	if _, err := decodeManifest(manifestData); err != nil {
		return fmt.Errorf("verify activated portal extension during update recovery: %w", err)
	}
	pendingMatches, err := digestRecordMatches(m.pendingRecordPath(), manifestData)
	if err != nil {
		return fmt.Errorf("verify pending Portal extension update record: %w", err)
	}
	if !pendingMatches {
		return fmt.Errorf("activated Portal extension does not match a pending update; preserving %s for recovery", backup)
	}
	if err := m.migrateLegacyStateFrom(backup); err != nil {
		return err
	}
	if err := m.migrateLegacyStateFrom(target); err != nil {
		return err
	}
	if err := m.writeInstallRecord(manifestData); err != nil {
		return err
	}
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("finish interrupted portal extension update: %w", err)
	}
	if err := os.Remove(m.pendingRecordPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear pending Portal extension update record: %w", err)
	}
	return nil
}

func (m *Manager) migrateLegacyStateFrom(extensionRoot string) error {
	legacy := filepath.Join(extensionRoot, legacyStateDir)
	for _, name := range []string{"preferences.json", "sources.json"} {
		source := filepath.Join(legacy, name)
		data, sourceInfo, exists, err := readStableRegularFile(source)
		if err != nil {
			return fmt.Errorf("read legacy Portal state %s: %w", name, err)
		}
		if !exists {
			continue
		}
		destination := filepath.Join(m.home, stateDirectory, Name, name)
		if !json.Valid(data) {
			return fmt.Errorf("legacy Portal state %s is not valid JSON", source)
		}
		current, destinationInfo, destinationExists, err := readStableRegularFile(destination)
		if err != nil {
			return fmt.Errorf("read Portal state destination: %w", err)
		}
		if destinationExists {
			if !json.Valid(current) {
				return fmt.Errorf("Portal state destination %s is not valid JSON; refusing to discard legacy state", destination)
			}
			if !sourceInfo.ModTime().After(destinationInfo.ModTime()) {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return fmt.Errorf("create Portal state directory: %w", err)
		}
		if err := writeAtomic(destination, data, defaultFileMode); err != nil {
			return fmt.Errorf("migrate legacy Portal state %s: %w", name, err)
		}
	}
	return nil
}

func readStableRegularFile(name string) ([]byte, fs.FileInfo, bool, error) {
	for attempt := 0; attempt < 5; attempt++ {
		before, err := os.Lstat(name)
		if os.IsNotExist(err) {
			return nil, nil, false, nil
		}
		if err != nil {
			return nil, nil, false, err
		}
		if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
			return nil, nil, false, fmt.Errorf("%s is not a safe regular file", name)
		}
		data, err := os.ReadFile(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, nil, false, err
		}
		after, err := os.Lstat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, nil, false, err
		}
		if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() {
			return nil, nil, false, fmt.Errorf("%s is not a safe regular file", name)
		}
		if os.SameFile(before, after) &&
			before.Size() == after.Size() &&
			before.ModTime().Equal(after.ModTime()) {
			return data, after, true, nil
		}
	}
	return nil, nil, false, fmt.Errorf("%s changed repeatedly while reading", name)
}

func (m *Manager) installRecordPath() string {
	return filepath.Join(m.home, stateDirectory, Name, installRecord)
}

func (m *Manager) pendingRecordPath() string {
	return filepath.Join(m.home, stateDirectory, Name, pendingRecord)
}

func (m *Manager) installRecordMatches(manifestData []byte) (bool, error) {
	return digestRecordMatches(m.installRecordPath(), manifestData)
}

func digestRecordMatches(recordPath string, manifestData []byte) (bool, error) {
	data, err := os.ReadFile(recordPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read Portal extension digest record: %w", err)
	}
	sum := sha256.Sum256(manifestData)
	return strings.TrimSpace(string(data)) == fmt.Sprintf("%x", sum), nil
}

func (m *Manager) writeInstallRecord(manifestData []byte) error {
	if err := m.writeDigestRecord(m.installRecordPath(), manifestData); err != nil {
		return fmt.Errorf("write Portal extension install record: %w", err)
	}
	return nil
}

func (m *Manager) writeDigestRecord(destination string, manifestData []byte) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create Portal state directory: %w", err)
	}
	sum := sha256.Sum256(manifestData)
	if err := writeAtomic(destination, []byte(fmt.Sprintf("%x\n", sum)), defaultFileMode); err != nil {
		return err
	}
	return nil
}

func writeAtomic(destination string, data []byte, mode fs.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".tmp-")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer func() { _ = os.Remove(temp) }()
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temp, destination)
}

func sameAssets(left, right []Asset) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func decodeManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.SchemaVersion != manifestSchema || manifest.Name != Name ||
		strings.TrimSpace(manifest.Version) == "" || strings.TrimSpace(manifest.Commit) == "" {
		return Manifest{}, errors.New("invalid portal extension manifest identity")
	}
	seen := map[string]bool{}
	for _, asset := range manifest.Assets {
		if !validRelativePath(asset.Path) || asset.SHA256 == "" || seen[asset.Path] {
			return Manifest{}, fmt.Errorf("invalid portal extension asset %q", asset.Path)
		}
		seen[asset.Path] = true
	}
	return manifest, nil
}

func validRelativePath(name string) bool {
	return name != "" && name == path.Clean(name) && name != "." &&
		!strings.HasPrefix(name, "../") && !strings.HasPrefix(name, "/") &&
		!strings.Contains(name, `\`)
}

// DetectCopilotApp reports whether the desktop Copilot app is installed.
func DetectCopilotApp() bool {
	home, _ := os.UserHomeDir()
	return DetectCopilotAppWith(runtime.GOOS, home, os.Getenv("LOCALAPPDATA"), exec.LookPath)
}

// DetectCopilotAppWith is the deterministic platform detection primitive.
func DetectCopilotAppWith(goos, home, localAppData string, lookPath func(string) (string, error)) bool {
	if lookPath != nil {
		if _, err := lookPath("github"); err == nil {
			return true
		}
	}
	var candidates []string
	switch goos {
	case "windows":
		if localAppData != "" {
			candidates = append(candidates, filepath.Join(localAppData, "Programs", "GitHub Copilot", "github.exe"))
		}
	case "darwin":
		candidates = append(candidates, "/Applications/GitHub Copilot.app")
		if home != "" {
			candidates = append(candidates, filepath.Join(home, "Applications", "GitHub Copilot.app"))
		}
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return true
		}
	}
	return false
}
