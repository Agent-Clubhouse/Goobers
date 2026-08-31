package agentkit

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/platform/durability"
)

// Repository is a validated configuration repository root.
type Repository struct {
	root string
}

const (
	updateTransactionPath    = ".goobers/.agent-toolkit-update.json"
	updateTransactionVersion = 2

	instructionReferenceBegin = "<!-- goobers:agent-toolkit:begin -->"
	instructionReferenceEnd   = "<!-- goobers:agent-toolkit:end -->"
)

type updateTransaction struct {
	Version int      `json:"version"`
	Changes []Change `json:"changes"`
}

// InstallResult describes toolkit and harness-reference changes.
type InstallResult struct {
	Installed          bool
	InstructionCreated bool
	InstructionUpdated bool
	InstructionPath    string
	Reference          string
}

// CheckReport describes installed identity, drift, and update availability.
type CheckReport struct {
	State                  string
	BundleVersion          string
	SourceBinaryVersion    string
	SourceBinaryCommit     string
	InstalledBundleVersion string
	InstalledSourceVersion string
	InstalledSourceCommit  string
	UpdateAvailable        bool
	Modified               []string
	Missing                []string
}

// ChangeKind identifies an update operation.
type ChangeKind string

// Supported update operations.
const (
	ChangeAdd    ChangeKind = "add"
	ChangeModify ChangeKind = "modify"
	ChangeDelete ChangeKind = "delete"
)

// Change is one reviewable product-owned file update.
type Change struct {
	Path    string
	Kind    ChangeKind
	Old     []byte
	New     []byte
	OldMode fs.FileMode
	Mode    fs.FileMode
}

// UpdatePlan contains changes and ownership conditions for an update.
type UpdatePlan struct {
	Changes        []Change
	ModifiedOwned  []string
	UserCollisions []string

	interruptedTransactionDigest string
}

type repositoryFile struct {
	data   []byte
	mode   fs.FileMode
	exists bool
}

// OpenRepository validates an unambiguous, symlink-free toolkit root. The
// folder may be versioned with Git, but a repository marker is not required.
func OpenRepository(target string) (*Repository, error) {
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("agent toolkit target path must not be empty")
	}
	for _, segment := range strings.FieldsFunc(filepath.ToSlash(target), func(r rune) bool { return r == '/' }) {
		if segment == ".." {
			return nil, fmt.Errorf("agent toolkit target path must not contain parent traversal")
		}
	}
	absolute, err := filepath.Abs(target)
	if err != nil {
		return nil, fmt.Errorf("resolve agent toolkit target: %w", err)
	}
	info, err := inspectTargetPath(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect agent toolkit target: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("agent toolkit target %s is not a directory", absolute)
	}
	return &Repository{root: absolute}, nil
}

// Install places a bundle and adds a harness reference without replacing user files.
func (r *Repository) Install(bundle Bundle, harness string) (InstallResult, error) {
	adapter, ok := bundle.Adapter(harness)
	if !ok {
		return InstallResult{}, fmt.Errorf("unsupported agent toolkit harness %q", harness)
	}
	instructionPath, err := r.securePath(adapter.InstructionTarget)
	if err != nil {
		return InstallResult{}, err
	}
	instructionInfo, instructionErr := os.Lstat(instructionPath)
	switch {
	case instructionErr == nil && instructionInfo.Mode()&os.ModeSymlink != 0:
		return InstallResult{}, fmt.Errorf("agent toolkit instruction target %s is a symbolic link", adapter.InstructionTarget)
	case instructionErr == nil && !instructionInfo.Mode().IsRegular():
		return InstallResult{}, fmt.Errorf("agent toolkit instruction target %s is not a regular file", adapter.InstructionTarget)
	case instructionErr != nil && !os.IsNotExist(instructionErr):
		return InstallResult{}, fmt.Errorf("inspect agent toolkit instruction target: %w", instructionErr)
	}

	manifest, manifestData, err := r.readManifest()
	installed := false
	switch {
	case err == nil:
		report, checkErr := r.check(bundle, manifest)
		if checkErr != nil {
			return InstallResult{}, checkErr
		}
		if report.State != "current" {
			return InstallResult{}, fmt.Errorf("agent toolkit is already installed with state %s; run `goobers agent-kit check` and `goobers agent-kit update`", report.State)
		}
		if !bytes.Equal(manifestData, bundle.ManifestJSON) {
			return InstallResult{}, fmt.Errorf("agent toolkit manifest is locally modified; run `goobers agent-kit update`")
		}
	case errors.Is(err, os.ErrNotExist):
		rootPath, pathErr := r.securePath(InstalledRoot)
		if pathErr != nil {
			return InstallResult{}, pathErr
		}
		if _, statErr := os.Lstat(rootPath); statErr == nil {
			return InstallResult{}, fmt.Errorf("%s exists without an installed manifest; refusing to claim or overwrite it", InstalledRoot)
		} else if !os.IsNotExist(statErr) {
			return InstallResult{}, fmt.Errorf("inspect agent toolkit root: %w", statErr)
		}
		if err := r.installBundle(bundle); err != nil {
			return InstallResult{}, err
		}
		installed = true
	default:
		return InstallResult{}, err
	}

	reference := fmt.Sprintf("Follow `%s` for Goobers-related work.", strings.TrimPrefix(adapter.Path, "payload/"))
	result := InstallResult{
		Installed:       installed,
		InstructionPath: adapter.InstructionTarget,
		Reference:       reference,
	}
	if os.IsNotExist(instructionErr) {
		content := []byte("# Goobers agent toolkit\n\n" + instructionReferenceBlock(reference))
		created, err := r.createFile(adapter.InstructionTarget, content, 0o644)
		if err != nil {
			return InstallResult{}, fmt.Errorf("write agent toolkit instruction reference: %w", err)
		}
		result.InstructionCreated = created
		if created {
			return result, nil
		}
		instructionInfo, err = os.Lstat(instructionPath)
		switch {
		case err != nil:
			return InstallResult{}, fmt.Errorf("inspect agent toolkit instruction target after concurrent creation: %w", err)
		case instructionInfo.Mode()&os.ModeSymlink != 0:
			return InstallResult{}, fmt.Errorf("agent toolkit instruction target %s is a symbolic link", adapter.InstructionTarget)
		case !instructionInfo.Mode().IsRegular():
			return InstallResult{}, fmt.Errorf("agent toolkit instruction target %s is not a regular file", adapter.InstructionTarget)
		}
	}

	instruction, exists, err := r.readFile(adapter.InstructionTarget)
	if err != nil {
		return InstallResult{}, err
	}
	if !exists {
		return InstallResult{}, fmt.Errorf("agent toolkit instruction target %s disappeared during installation", adapter.InstructionTarget)
	}
	updated, changed, err := addInstructionReference(instruction, reference)
	if err != nil {
		return InstallResult{}, fmt.Errorf("update agent toolkit instruction reference: %w", err)
	}
	if changed {
		if err := r.writeFile(adapter.InstructionTarget, updated, instructionInfo.Mode().Perm()); err != nil {
			return InstallResult{}, fmt.Errorf("write agent toolkit instruction reference: %w", err)
		}
		result.InstructionUpdated = true
	}
	return result, nil
}

// Check compares installed assets and identity with the supplied bundle.
func (r *Repository) Check(bundle Bundle) (CheckReport, error) {
	manifest, manifestData, err := r.readManifest()
	if errors.Is(err, os.ErrNotExist) {
		return CheckReport{
			State:               "missing-manifest",
			BundleVersion:       bundle.Manifest.BundleVersion,
			SourceBinaryVersion: bundle.Manifest.Producer.Version,
			SourceBinaryCommit:  bundle.Manifest.Producer.Commit,
			UpdateAvailable:     true,
		}, nil
	}
	if err != nil {
		return CheckReport{}, err
	}
	report, err := r.check(bundle, manifest)
	if err != nil {
		return CheckReport{}, err
	}
	canonical, err := marshalManifest(manifest)
	if err != nil {
		return CheckReport{}, err
	}
	manifestModified := semanticManifestDrift(manifest, bundle.Manifest) || !bytes.Equal(manifestData, canonical)
	if !sameManifestRelease(manifest, bundle.Manifest) {
		if committed, commitErr := r.readCommittedManifest(); commitErr == nil && !bytes.Equal(manifestData, committed) {
			manifestModified = true
		}
	}
	if manifestModified {
		report.Modified = append(report.Modified, InstalledManifestPath)
		sort.Strings(report.Modified)
		if report.State == "current" {
			report.State = "modified"
		}
	}
	return report, nil
}

// PlanUpdate computes product-owned changes without mutating the repository.
func (r *Repository) PlanUpdate(bundle Bundle) (UpdatePlan, error) {
	installed, manifestData, err := r.readManifest()
	if errors.Is(err, os.ErrNotExist) {
		return UpdatePlan{}, fmt.Errorf("agent toolkit manifest is missing; install into an empty target or restore the manifest before updating")
	}
	if err != nil {
		return UpdatePlan{}, err
	}
	transactionData, transactionExists, err := r.readFile(updateTransactionPath)
	if err != nil {
		return UpdatePlan{}, err
	}
	if transactionExists {
		return r.planInterruptedUpdate(bundle, installed, manifestData, transactionData)
	}

	ownershipManifest, manifestModified, err := r.trustedOwnershipManifest(bundle, installed, manifestData)
	if err != nil {
		return UpdatePlan{}, err
	}
	oldAssets := make(map[string]Asset, len(ownershipManifest.Assets))
	for _, asset := range ownershipManifest.Assets {
		installedPath, err := InstalledAssetPath(asset.Path)
		if err != nil {
			return UpdatePlan{}, err
		}
		oldAssets[installedPath] = asset
	}

	var plan UpdatePlan
	currentPaths := make([]string, 0, len(bundle.Files))
	for installedPath := range bundle.Files {
		currentPaths = append(currentPaths, installedPath)
	}
	sort.Strings(currentPaths)
	for _, installedPath := range currentPaths {
		file := bundle.Files[installedPath]
		state, err := r.readFileState(installedPath)
		if err != nil {
			return UpdatePlan{}, err
		}
		oldAsset, previouslyOwned := oldAssets[installedPath]
		if !previouslyOwned && state.exists {
			plan.UserCollisions = append(plan.UserCollisions, installedPath)
			continue
		}
		if !state.exists {
			plan.Changes = append(plan.Changes, Change{
				Path: installedPath,
				Kind: ChangeAdd,
				New:  append([]byte(nil), file.Data...),
				Mode: file.Mode,
			})
			continue
		}
		if bytes.Equal(state.data, file.Data) && requiredModeMatches(state.mode, file.Mode) {
			continue
		}
		plan.Changes = append(plan.Changes, Change{
			Path:    installedPath,
			Kind:    ChangeModify,
			Old:     state.data,
			New:     append([]byte(nil), file.Data...),
			OldMode: state.mode.Perm(),
			Mode:    file.Mode,
		})
		if previouslyOwned {
			oldMode, err := parseAssetMode(oldAsset.Mode)
			if err != nil {
				return UpdatePlan{}, err
			}
			if digest(state.data) != oldAsset.SHA256 || !requiredModeMatches(state.mode, oldMode) {
				plan.ModifiedOwned = append(plan.ModifiedOwned, installedPath)
			}
		}
	}

	oldPaths := make([]string, 0, len(oldAssets))
	for installedPath := range oldAssets {
		oldPaths = append(oldPaths, installedPath)
	}
	sort.Strings(oldPaths)
	for _, installedPath := range oldPaths {
		if _, stillOwned := bundle.Files[installedPath]; stillOwned {
			continue
		}
		state, err := r.readFileState(installedPath)
		if err != nil {
			return UpdatePlan{}, err
		}
		if !state.exists {
			continue
		}
		plan.Changes = append(plan.Changes, Change{
			Path:    installedPath,
			Kind:    ChangeDelete,
			Old:     state.data,
			OldMode: state.mode.Perm(),
		})
		oldMode, err := parseAssetMode(oldAssets[installedPath].Mode)
		if err != nil {
			return UpdatePlan{}, err
		}
		if digest(state.data) != oldAssets[installedPath].SHA256 || !requiredModeMatches(state.mode, oldMode) {
			plan.ModifiedOwned = append(plan.ModifiedOwned, installedPath)
		}
	}

	if !bytes.Equal(manifestData, bundle.ManifestJSON) {
		if manifestModified {
			plan.ModifiedOwned = append(plan.ModifiedOwned, InstalledManifestPath)
		}
		plan.Changes = append(plan.Changes, Change{
			Path: InstalledManifestPath,
			Kind: ChangeModify,
			Old:  manifestData,
			New:  append([]byte(nil), bundle.ManifestJSON...),
			Mode: 0o644,
		})
	}
	sort.Strings(plan.ModifiedOwned)
	sort.Strings(plan.UserCollisions)
	return plan, nil
}

// ApplyUpdate writes a plan after enforcing its ownership acknowledgements.
func (r *Repository) ApplyUpdate(plan UpdatePlan, replaceModified bool) error {
	if len(plan.UserCollisions) > 0 {
		return fmt.Errorf("update would overwrite user-owned files: %s", strings.Join(plan.UserCollisions, ", "))
	}
	if len(plan.ModifiedOwned) > 0 && !replaceModified {
		return fmt.Errorf("locally modified product-owned files require --replace-modified: %s", strings.Join(plan.ModifiedOwned, ", "))
	}
	if len(plan.Changes) == 0 {
		return nil
	}
	if plan.interruptedTransactionDigest != "" {
		transactionData, exists, err := r.readFile(updateTransactionPath)
		if err != nil {
			return err
		}
		if !exists || digest(transactionData) != plan.interruptedTransactionDigest {
			return fmt.Errorf("interrupted agent toolkit update changed after planning; refusing to resume it")
		}
		if err := r.validateUpdateChanges(plan.Changes, true); err != nil {
			return err
		}
		return r.resumeUpdate(plan.Changes)
	}
	if err := r.validateUpdatePlan(plan); err != nil {
		return err
	}
	if err := r.writeUpdateTransaction(plan.Changes); err != nil {
		return err
	}
	return r.resumeUpdate(plan.Changes)
}

func (r *Repository) validateUpdatePlan(plan UpdatePlan) error {
	return r.validateUpdateChanges(plan.Changes, false)
}

func (r *Repository) validateUpdateChanges(changes []Change, recovering bool) error {
	seen := make(map[string]struct{}, len(changes))
	for _, change := range changes {
		if change.Path != InstalledRoot && !strings.HasPrefix(change.Path, InstalledRoot+"/") {
			return fmt.Errorf("update path %q is outside the agent toolkit root", change.Path)
		}
		if _, duplicate := seen[change.Path]; duplicate {
			return fmt.Errorf("update contains duplicate change for %s", change.Path)
		}
		seen[change.Path] = struct{}{}
		if change.Path == InstalledManifestPath && change.Kind != ChangeModify {
			return fmt.Errorf("agent toolkit manifest update must be a modification")
		}
		state, err := r.readFileState(change.Path)
		if err != nil {
			return err
		}
		switch change.Kind {
		case ChangeAdd:
			complete := state.exists &&
				bytes.Equal(state.data, change.New) &&
				requiredModeMatches(state.mode, change.Mode)
			if state.exists && (!recovering || !complete) {
				return fmt.Errorf("%s appeared after update planning; refusing to overwrite it", change.Path)
			}
		case ChangeModify:
			original := state.exists &&
				bytes.Equal(state.data, change.Old) &&
				requiredModeMatches(state.mode, change.OldMode)
			complete := state.exists &&
				bytes.Equal(state.data, change.New) &&
				requiredModeMatches(state.mode, change.Mode)
			if !original && (!recovering || !complete) {
				return fmt.Errorf("%s changed after update planning; refusing to overwrite it", change.Path)
			}
			if change.Path == InstalledManifestPath {
				if _, err := DecodeManifest(change.New); err != nil {
					return fmt.Errorf("validate updated agent toolkit manifest: %w", err)
				}
			}
		case ChangeDelete:
			original := state.exists &&
				bytes.Equal(state.data, change.Old) &&
				requiredModeMatches(state.mode, change.OldMode)
			if state.exists && !original {
				return fmt.Errorf("%s changed after update planning; refusing to delete it", change.Path)
			}
			if !recovering && !state.exists {
				return fmt.Errorf("%s changed after update planning; refusing to delete it", change.Path)
			}
		default:
			return fmt.Errorf("unsupported agent toolkit update change %q", change.Kind)
		}
	}
	return nil
}

func (r *Repository) planInterruptedUpdate(
	bundle Bundle,
	installed Manifest,
	manifestData, transactionData []byte,
) (UpdatePlan, error) {
	var transaction updateTransaction
	if err := json.Unmarshal(transactionData, &transaction); err != nil {
		return UpdatePlan{}, fmt.Errorf("decode interrupted agent toolkit update: %w", err)
	}
	if transaction.Version != updateTransactionVersion || len(transaction.Changes) == 0 {
		return UpdatePlan{}, fmt.Errorf("interrupted agent toolkit update has an unsupported transaction")
	}

	sourceManifest := installed
	sourceManifestData := manifestData
	manifestAlreadyUpdated := false
	var manifestChange *Change
	for index := range transaction.Changes {
		change := &transaction.Changes[index]
		if change.Path != InstalledManifestPath {
			continue
		}
		if manifestChange != nil {
			return UpdatePlan{}, fmt.Errorf("interrupted agent toolkit update contains duplicate manifest changes")
		}
		manifestChange = change
	}
	if manifestChange != nil {
		if manifestChange.Kind != ChangeModify ||
			!bytes.Equal(manifestChange.New, bundle.ManifestJSON) ||
			manifestChange.Mode != 0o644 {
			return UpdatePlan{}, fmt.Errorf("interrupted agent toolkit update does not target the current bundle manifest")
		}
		var err error
		sourceManifest, err = DecodeManifest(manifestChange.Old)
		if err != nil {
			return UpdatePlan{}, fmt.Errorf("decode interrupted agent toolkit source manifest: %w", err)
		}
		sourceManifestData = manifestChange.Old
		if !bytes.Equal(manifestData, manifestChange.Old) &&
			!bytes.Equal(manifestData, manifestChange.New) {
			return UpdatePlan{}, fmt.Errorf("installed manifest changed during the interrupted agent toolkit update")
		}
		manifestAlreadyUpdated = bytes.Equal(manifestData, manifestChange.New)
	} else if !bytes.Equal(manifestData, bundle.ManifestJSON) {
		return UpdatePlan{}, fmt.Errorf("interrupted agent toolkit update omits the required manifest change")
	}

	ownershipManifest, sourceManifestModified, err := r.trustedOwnershipManifest(
		bundle,
		sourceManifest,
		sourceManifestData,
	)
	if err != nil {
		return UpdatePlan{}, err
	}
	oldAssets := make(map[string]Asset, len(ownershipManifest.Assets))
	for _, asset := range ownershipManifest.Assets {
		installedPath, err := InstalledAssetPath(asset.Path)
		if err != nil {
			return UpdatePlan{}, err
		}
		oldAssets[installedPath] = asset
	}

	plan := UpdatePlan{
		Changes:                      transaction.Changes,
		interruptedTransactionDigest: digest(transactionData),
	}
	if sourceManifestModified {
		plan.ModifiedOwned = append(plan.ModifiedOwned, InstalledManifestPath)
	}

	seen := make(map[string]struct{}, len(transaction.Changes))
	for _, change := range transaction.Changes {
		if change.Path != InstalledRoot && !strings.HasPrefix(change.Path, InstalledRoot+"/") {
			return UpdatePlan{}, fmt.Errorf("interrupted update path %q is outside the agent toolkit root", change.Path)
		}
		if _, duplicate := seen[change.Path]; duplicate {
			return UpdatePlan{}, fmt.Errorf("interrupted update contains duplicate change for %s", change.Path)
		}
		seen[change.Path] = struct{}{}
		if change.Path == InstalledManifestPath {
			continue
		}

		oldAsset, previouslyOwned := oldAssets[change.Path]
		target, targetOwned := bundle.Files[change.Path]
		switch change.Kind {
		case ChangeAdd:
			if !targetOwned || len(change.Old) != 0 || change.OldMode != 0 ||
				!bytes.Equal(change.New, target.Data) || change.Mode != target.Mode {
				return UpdatePlan{}, fmt.Errorf("interrupted update add %s is not an exact current-bundle addition or owned-file restoration", change.Path)
			}
		case ChangeModify:
			if !previouslyOwned || !targetOwned ||
				!bytes.Equal(change.New, target.Data) || change.Mode != target.Mode ||
				change.OldMode != change.OldMode.Perm() {
				return UpdatePlan{}, fmt.Errorf("interrupted update modification %s is not an exact manifest-owned current-bundle change", change.Path)
			}
			oldMode, err := parseAssetMode(oldAsset.Mode)
			if err != nil {
				return UpdatePlan{}, err
			}
			if digest(change.Old) != oldAsset.SHA256 || !requiredModeMatches(change.OldMode, oldMode) {
				plan.ModifiedOwned = append(plan.ModifiedOwned, change.Path)
			}
		case ChangeDelete:
			if !previouslyOwned || targetOwned || len(change.New) != 0 || change.Mode != 0 ||
				change.OldMode != change.OldMode.Perm() {
				return UpdatePlan{}, fmt.Errorf("interrupted update deletion %s is not an obsolete manifest-owned asset", change.Path)
			}
			oldMode, err := parseAssetMode(oldAsset.Mode)
			if err != nil {
				return UpdatePlan{}, err
			}
			if digest(change.Old) != oldAsset.SHA256 || !requiredModeMatches(change.OldMode, oldMode) {
				plan.ModifiedOwned = append(plan.ModifiedOwned, change.Path)
			}
		default:
			return UpdatePlan{}, fmt.Errorf("unsupported interrupted agent toolkit update change %q", change.Kind)
		}
		if manifestAlreadyUpdated {
			state, err := r.readFileState(change.Path)
			if err != nil {
				return UpdatePlan{}, err
			}
			complete := change.Kind == ChangeDelete && !state.exists ||
				change.Kind != ChangeDelete &&
					state.exists &&
					bytes.Equal(state.data, change.New) &&
					requiredModeMatches(state.mode, change.Mode)
			if !complete {
				return UpdatePlan{}, fmt.Errorf(
					"interrupted update has an incomplete asset change after its manifest was installed: %s",
					change.Path,
				)
			}
		}
	}
	if err := r.validateUpdateChanges(transaction.Changes, true); err != nil {
		return UpdatePlan{}, fmt.Errorf("validate interrupted agent toolkit update: %w", err)
	}
	sort.Strings(plan.ModifiedOwned)
	plan.ModifiedOwned = compactStrings(plan.ModifiedOwned)
	return plan, nil
}

func (r *Repository) writeUpdateTransaction(changes []Change) error {
	data, err := json.Marshal(updateTransaction{
		Version: updateTransactionVersion,
		Changes: changes,
	})
	if err != nil {
		return fmt.Errorf("encode agent toolkit update transaction: %w", err)
	}
	data = append(data, '\n')
	created, err := r.createFile(updateTransactionPath, data, 0o644)
	if err != nil {
		return fmt.Errorf("write agent toolkit update transaction: %w", err)
	}
	if !created {
		return fmt.Errorf("agent toolkit update transaction appeared during planning")
	}
	return nil
}

func (r *Repository) resumeUpdate(changes []Change) error {
	for _, manifest := range []bool{false, true} {
		for _, change := range changes {
			if (change.Path == InstalledManifestPath) != manifest {
				continue
			}
			if err := r.applyUpdateChange(change); err != nil {
				return err
			}
		}
	}
	transactionPath, err := r.securePath(updateTransactionPath)
	if err != nil {
		return err
	}
	if err := os.Remove(transactionPath); err != nil {
		return fmt.Errorf("remove completed agent toolkit update transaction: %w", err)
	}
	if err := durability.SyncDir(filepath.Dir(transactionPath)); err != nil {
		return fmt.Errorf("sync completed agent toolkit update transaction: %w", err)
	}
	return nil
}

func (r *Repository) applyUpdateChange(change Change) error {
	state, err := r.readFileState(change.Path)
	if err != nil {
		return err
	}
	switch change.Kind {
	case ChangeAdd:
		if state.exists {
			if bytes.Equal(state.data, change.New) && requiredModeMatches(state.mode, change.Mode) {
				return nil
			}
			return fmt.Errorf("%s changed during update recovery; refusing to overwrite it", change.Path)
		}
		created, err := r.createFile(change.Path, change.New, change.Mode)
		if err != nil {
			return err
		}
		if !created {
			return fmt.Errorf("%s appeared during update recovery; refusing to overwrite it", change.Path)
		}
	case ChangeModify:
		if state.exists &&
			bytes.Equal(state.data, change.New) &&
			requiredModeMatches(state.mode, change.Mode) {
			return nil
		}
		if !state.exists ||
			!bytes.Equal(state.data, change.Old) ||
			!requiredModeMatches(state.mode, change.OldMode) {
			return fmt.Errorf("%s changed during update recovery; refusing to overwrite it", change.Path)
		}
		if err := r.writeFile(change.Path, change.New, change.Mode); err != nil {
			return err
		}
	case ChangeDelete:
		if !state.exists {
			return nil
		}
		if !bytes.Equal(state.data, change.Old) || !requiredModeMatches(state.mode, change.OldMode) {
			return fmt.Errorf("%s changed during update recovery; refusing to delete it", change.Path)
		}
		fullPath, err := r.securePath(change.Path)
		if err != nil {
			return err
		}
		if err := os.Remove(fullPath); err != nil {
			return fmt.Errorf("remove obsolete agent toolkit asset %s: %w", change.Path, err)
		}
		if err := durability.SyncDir(filepath.Dir(fullPath)); err != nil {
			return fmt.Errorf("sync removal of obsolete agent toolkit asset %s: %w", change.Path, err)
		}
		r.removeEmptyParents(filepath.Dir(fullPath))
	default:
		return fmt.Errorf("unsupported agent toolkit update change %q", change.Kind)
	}
	return nil
}

func (r *Repository) check(bundle Bundle, installed Manifest) (CheckReport, error) {
	report := CheckReport{
		BundleVersion:          bundle.Manifest.BundleVersion,
		SourceBinaryVersion:    bundle.Manifest.Producer.Version,
		SourceBinaryCommit:     bundle.Manifest.Producer.Commit,
		InstalledBundleVersion: installed.BundleVersion,
		InstalledSourceVersion: installed.Producer.Version,
		InstalledSourceCommit:  installed.Producer.Commit,
	}
	report.UpdateAvailable = !manifestsMatch(installed, bundle.Manifest)
	for _, asset := range installed.Assets {
		installedPath, err := InstalledAssetPath(asset.Path)
		if err != nil {
			return CheckReport{}, err
		}
		state, err := r.readFileState(installedPath)
		if err != nil {
			return CheckReport{}, err
		}
		if !state.exists {
			report.Missing = append(report.Missing, installedPath)
			continue
		}
		requiredMode, err := parseAssetMode(asset.Mode)
		if err != nil {
			return CheckReport{}, err
		}
		if digest(state.data) != asset.SHA256 || !requiredModeMatches(state.mode, requiredMode) {
			report.Modified = append(report.Modified, installedPath)
		}
	}
	sort.Strings(report.Modified)
	sort.Strings(report.Missing)
	switch {
	case report.UpdateAvailable:
		report.State = "upgrade-available"
	case len(report.Modified) > 0:
		report.State = "modified"
	case len(report.Missing) > 0:
		report.State = "missing"
	default:
		report.State = "current"
	}
	return report, nil
}

func (r *Repository) installBundle(bundle Bundle) error {
	goobersPath, err := r.securePath(".goobers")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(goobersPath, 0o755); err != nil {
		return fmt.Errorf("create .goobers directory: %w", err)
	}
	staging, err := os.MkdirTemp(goobersPath, ".agent-toolkit-install-")
	if err != nil {
		return fmt.Errorf("create agent toolkit staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	paths := make([]string, 0, len(bundle.Files))
	for installedPath := range bundle.Files {
		paths = append(paths, installedPath)
	}
	sort.Strings(paths)
	for _, installedPath := range paths {
		file := bundle.Files[installedPath]
		relative := strings.TrimPrefix(installedPath, InstalledRoot+"/")
		stagedPath := filepath.Join(staging, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(stagedPath), 0o755); err != nil {
			return fmt.Errorf("create agent toolkit staging directory: %w", err)
		}
		if err := os.WriteFile(stagedPath, file.Data, file.Mode); err != nil {
			return fmt.Errorf("stage agent toolkit asset %s: %w", installedPath, err)
		}
		if err := os.Chmod(stagedPath, file.Mode); err != nil {
			return fmt.Errorf("set staged agent toolkit asset mode %s: %w", installedPath, err)
		}
	}
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), bundle.ManifestJSON, 0o644); err != nil {
		return fmt.Errorf("stage agent toolkit manifest: %w", err)
	}
	finalPath := filepath.Join(goobersPath, "agent-toolkit")
	if _, err := os.Lstat(finalPath); err == nil {
		return fmt.Errorf("%s appeared during installation; refusing to replace it", InstalledRoot)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect agent toolkit install destination: %w", err)
	}
	if err := os.Rename(staging, finalPath); err != nil {
		return fmt.Errorf("install agent toolkit: %w", err)
	}
	return nil
}

func (r *Repository) readManifest() (Manifest, []byte, error) {
	data, exists, err := r.readFile(InstalledManifestPath)
	if err != nil {
		return Manifest{}, nil, err
	}
	if !exists {
		return Manifest{}, nil, os.ErrNotExist
	}
	manifest, err := DecodeManifest(data)
	if err != nil {
		return Manifest{}, nil, err
	}
	return manifest, data, nil
}

func (r *Repository) trustedOwnershipManifest(bundle Bundle, candidate Manifest, candidateData []byte) (Manifest, bool, error) {
	if sameManifestRelease(candidate, bundle.Manifest) {
		return bundle.Manifest, !bytes.Equal(candidateData, bundle.ManifestJSON), nil
	}
	committedData, err := r.readCommittedManifest()
	if err != nil {
		return Manifest{}, false, fmt.Errorf(
			"agent toolkit ownership cannot be verified across releases; commit %s before updating: %w",
			InstalledManifestPath,
			err,
		)
	}
	committed, err := DecodeManifest(committedData)
	if err != nil {
		return Manifest{}, false, fmt.Errorf("decode checked-in agent toolkit manifest: %w", err)
	}
	return committed, !bytes.Equal(candidateData, committedData), nil
}

func (r *Repository) readCommittedManifest() ([]byte, error) {
	cmd := exec.Command("git", "--no-pager", "show", "--no-ext-diff", "--no-textconv", "HEAD:"+InstalledManifestPath)
	cmd.Dir = r.root
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read %s from HEAD: %w", InstalledManifestPath, err)
	}
	return data, nil
}

func (r *Repository) readFile(relative string) ([]byte, bool, error) {
	state, err := r.readFileState(relative)
	return state.data, state.exists, err
}

func (r *Repository) readFileState(relative string) (repositoryFile, error) {
	fullPath, err := r.securePath(relative)
	if err != nil {
		return repositoryFile{}, err
	}
	info, err := os.Lstat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return repositoryFile{}, nil
		}
		return repositoryFile{}, fmt.Errorf("inspect agent toolkit path %s: %w", relative, err)
	}
	if !info.Mode().IsRegular() {
		return repositoryFile{}, fmt.Errorf("agent toolkit path %s is not a regular file", relative)
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return repositoryFile{}, fmt.Errorf("read agent toolkit path %s: %w", relative, err)
	}
	return repositoryFile{data: data, mode: info.Mode().Perm(), exists: true}, nil
}

func (r *Repository) writeFile(relative string, data []byte, mode fs.FileMode) error {
	fullPath, err := r.securePath(relative)
	if err != nil {
		return err
	}
	parent := filepath.Dir(fullPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create parent directory for %s: %w", relative, err)
	}
	fullPath, err = r.securePath(relative)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(parent, "."+filepath.Base(fullPath)+"-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", relative, err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return fmt.Errorf("set temporary file mode for %s: %w", relative, err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary file for %s: %w", relative, err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync temporary file for %s: %w", relative, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary file for %s: %w", relative, err)
	}
	if err := durability.ReplaceFile(tempPath, fullPath); err != nil {
		return fmt.Errorf("write agent toolkit path %s: %w", relative, err)
	}
	if err := durability.SyncDir(parent); err != nil {
		return fmt.Errorf("sync parent directory for %s: %w", relative, err)
	}
	return nil
}

func (r *Repository) createFile(relative string, data []byte, mode fs.FileMode) (bool, error) {
	fullPath, err := r.securePath(relative)
	if err != nil {
		return false, err
	}
	parent := filepath.Dir(fullPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return false, fmt.Errorf("create parent directory for %s: %w", relative, err)
	}
	fullPath, err = r.securePath(relative)
	if err != nil {
		return false, err
	}
	file, err := os.OpenFile(fullPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if os.IsExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("create agent toolkit path %s: %w", relative, err)
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(fullPath)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return false, fmt.Errorf("set agent toolkit path mode %s: %w", relative, err)
	}
	if _, err := file.Write(data); err != nil {
		return false, fmt.Errorf("write agent toolkit path %s: %w", relative, err)
	}
	if err := file.Sync(); err != nil {
		return false, fmt.Errorf("sync agent toolkit path %s: %w", relative, err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("close agent toolkit path %s: %w", relative, err)
	}
	if err := durability.SyncDir(parent); err != nil {
		return false, fmt.Errorf("sync parent directory for %s: %w", relative, err)
	}
	complete = true
	return true, nil
}

func (r *Repository) securePath(relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, `\`) {
		return "", fmt.Errorf("unsafe agent toolkit path %q", relative)
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean != filepath.FromSlash(relative) ||
		clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe agent toolkit path %q", relative)
	}
	fullPath := filepath.Join(r.root, clean)
	back, err := filepath.Rel(r.root, fullPath)
	if err != nil || back != clean {
		return "", fmt.Errorf("agent toolkit path %q escapes repository root", relative)
	}

	current := r.root
	parts := strings.Split(clean, string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			break
		}
		if statErr != nil {
			return "", fmt.Errorf("inspect agent toolkit path %s: %w", relative, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("agent toolkit path %s traverses symbolic link %s", relative, strings.Join(parts[:index+1], "/"))
		}
		if index < len(parts)-1 && !info.IsDir() {
			return "", fmt.Errorf("agent toolkit path %s traverses non-directory %s", relative, strings.Join(parts[:index+1], "/"))
		}
	}
	return fullPath, nil
}

func (r *Repository) removeEmptyParents(directory string) {
	stop := filepath.Join(r.root, filepath.FromSlash(InstalledRoot))
	for directory != stop && strings.HasPrefix(directory, stop+string(filepath.Separator)) {
		if err := os.Remove(directory); err != nil {
			return
		}
		directory = filepath.Dir(directory)
	}
}

func inspectTargetPath(absolute string) (fs.FileInfo, error) {
	root := filepath.VolumeName(absolute) + string(filepath.Separator)
	relative, err := filepath.Rel(root, absolute)
	if err != nil {
		return nil, err
	}
	current := root
	info, err := os.Lstat(current)
	if err != nil {
		return nil, err
	}
	if relative == "." {
		return info, nil
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err = os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("target path traverses symbolic link %s", current)
		}
	}
	return info, nil
}

func manifestsMatch(installed, current Manifest) bool {
	return reflect.DeepEqual(installed, current)
}

func sameManifestRelease(installed, current Manifest) bool {
	return installed.SchemaVersion == current.SchemaVersion &&
		installed.BundleVersion == current.BundleVersion &&
		installed.Producer == current.Producer
}

func semanticManifestDrift(installed, current Manifest) bool {
	return sameManifestRelease(installed, current) && !manifestsMatch(installed, current)
}

func instructionReferenceBlock(reference string) string {
	return instructionReferenceBegin + "\n" + reference + "\n" + instructionReferenceEnd + "\n"
}

func addInstructionReference(content []byte, reference string) ([]byte, bool, error) {
	if bytes.Contains(content, []byte(reference)) {
		return content, false, nil
	}
	begin := bytes.Index(content, []byte(instructionReferenceBegin))
	end := bytes.Index(content, []byte(instructionReferenceEnd))
	if begin >= 0 || end >= 0 {
		return nil, false, fmt.Errorf("existing managed reference block is incomplete or locally modified")
	}

	updated := append([]byte(nil), content...)
	if len(updated) > 0 && updated[len(updated)-1] != '\n' {
		updated = append(updated, '\n')
	}
	if len(updated) > 0 && !bytes.HasSuffix(updated, []byte("\n\n")) {
		updated = append(updated, '\n')
	}
	updated = append(updated, instructionReferenceBlock(reference)...)
	return updated, true, nil
}

func requiredModeMatches(actual, required fs.FileMode) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	return actual.Perm()&0o111 == required.Perm()&0o111
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	compacted := values[:1]
	for _, value := range values[1:] {
		if value != compacted[len(compacted)-1] {
			compacted = append(compacted, value)
		}
	}
	return compacted
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}
