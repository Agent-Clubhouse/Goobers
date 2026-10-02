package harness

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gitexclude"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/workflow"
)

const skillStateDir = ".goobers/skills"
const skillManifestPath = skillStateDir + "/snapshot.json"

// WithSkills supplies captured package bytes, independently of prompt text and
// the adapter's diagnostic name (which can differ from its configured name).
func WithSkills(harness apiv1.Harness, packages map[string][]workflow.SkillFile) Option {
	return func(e *Executor) {
		e.skillsHarness = harness
		e.skills = make(map[string][]workflow.SkillFile, len(packages))
		for name, files := range packages {
			e.skills[name] = append([]workflow.SkillFile(nil), files...)
		}
	}
}

func skillsDirectory(harness apiv1.Harness) (string, error) {
	switch harness {
	case apiv1.HarnessCopilot:
		return ".github/skills", nil
	case apiv1.HarnessClaudeCode:
		return ".claude/skills", nil
	case apiv1.HarnessCodex:
		return ".agents/skills", nil
	default:
		return "", fmt.Errorf("unsupported skills harness %q", harness)
	}
}

type skillManifest struct {
	ID    string
	Paths []string
	Git   bool
}

type skillSnapshot struct {
	root      *os.Root
	workspace string
	held      *lock.Handle
	manifest  skillManifest
}

func (e *Executor) prepareSkills(ctx context.Context, workspace string) (_ *skillSnapshot, err error) {
	if e.skillsHarness == "" && len(e.skills) == 0 {
		return nil, nil
	}
	directory, err := skillsDirectory(e.skillsHarness)
	if err != nil {
		return nil, err
	}
	if err := validateSkillFiles(e.skills); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	snapshot := &skillSnapshot{root: root, workspace: workspace}
	defer func() {
		if err != nil {
			err = errors.Join(err, snapshot.Close())
		}
	}()
	if err := snapshot.reserveState(ctx); err != nil {
		return nil, err
	}
	if err := snapshot.acquire(); err != nil {
		return nil, err
	}

	if err := snapshot.recover(); err != nil {
		return nil, err
	}
	if len(e.skills) == 0 {
		return snapshot, nil
	}
	if err := snapshot.install(ctx, directory, e.skills); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *skillSnapshot) reserveState(ctx context.Context) error {
	if !isGitWorkspace(ctx, s.workspace) {
		return ctx.Err()
	}
	if err := s.refuseTracked(ctx, skillStateDir); err != nil {
		return err
	}
	return gitexclude.Ensure(ctx, s.workspace, gitexclude.Pattern{Line: "/.goobers/"})
}

func (s *skillSnapshot) acquire() error {
	if err := skillMkdirAll(s.root, skillStateDir); err != nil {
		return err
	}
	f, err := s.root.OpenFile(skillStateDir+"/lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		_ = f.Close()
	} else if !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := s.root.Lstat(skillStateDir + "/lock")
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("skill lock is not a regular file")
	}
	s.held, err = lock.TryAcquireExistingInRoot(s.root, skillStateDir+"/lock")
	return err
}

func (s *skillSnapshot) install(ctx context.Context, directory string, packages map[string][]workflow.SkillFile) error {
	var targets []string
	for name := range packages {
		target := directory + "/" + name
		if _, err := s.root.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("skill workspace path already exists: %s", target)
		}
		targets = append(targets, target)
	}
	sort.Strings(targets)
	manifest := skillManifest{ID: rand.Text(), Paths: targets, Git: isGitWorkspace(ctx, s.workspace)}
	if manifest.Git {
		if err := s.refuseTracked(ctx, targets...); err != nil {
			return err
		}
	}
	if err := skillMkdirAll(s.root, directory); err != nil {
		return err
	}
	if err := s.writeManifest(manifest); err != nil {
		return err
	}
	if manifest.Git {
		var patterns []gitexclude.Pattern
		for _, target := range targets {
			p, _ := gitexclude.Anchored(target)
			patterns = append(patterns, p)
		}
		if err := gitexclude.AddScoped(ctx, s.workspace, manifest.ID, patterns...); err != nil {
			return err
		}
	}
	if err := s.writePackages(directory, packages); err != nil {
		return err
	}
	if manifest.Git {
		visible, err := skillGit(ctx, s.workspace, append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--"}, targets...)...)
		if err != nil {
			return err
		}
		if len(visible) > 0 {
			return errors.New("repository ignore rules expose skill snapshot files")
		}
	}
	return nil
}

func (s *skillSnapshot) writePackages(directory string, packages map[string][]workflow.SkillFile) error {
	for name, files := range packages {
		for _, file := range files {
			target := directory + "/" + name + "/" + file.Path
			if err := skillMkdirAll(s.root, path.Dir(target)); err != nil {
				return err
			}
			if err := s.root.WriteFile(target, []byte(file.Content), 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSkillFiles(packages map[string][]workflow.SkillFile) error {
	names := make(map[string]bool)
	for name, files := range packages {
		if names[strings.ToLower(name)] || !safeSkillPath(name) || strings.Contains(name, "/") {
			return fmt.Errorf("unsafe skill name %q", name)
		}
		names[strings.ToLower(name)] = true
		seen := make(map[string]bool)
		for _, file := range files {
			if !safeSkillPath(file.Path) || seen[strings.ToLower(file.Path)] {
				return fmt.Errorf("unsafe or duplicate skill path %q", file.Path)
			}
			seen[strings.ToLower(file.Path)] = true
		}
	}
	return nil
}

func safeSkillPath(value string) bool {
	if !fs.ValidPath(value) || value == "." || strings.ContainsAny(value, "\\:\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

// Refuse symlinks even when they remain within the workspace: repository-owned
// aliases must not redirect snapshot creation or stale cleanup.
func skillMkdirAll(root *os.Root, relative string) error {
	current := ""
	for _, part := range strings.Split(relative, "/") {
		current = path.Join(current, part)
		if err := root.Mkdir(current, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("skill directory %q is not a directory", current)
		}
	}
	return nil
}

func (s *skillSnapshot) recover() error {
	info, err := s.root.Lstat(skillManifestPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("skill manifest is not a regular file")
	}
	data, err := s.root.ReadFile(skillManifestPath)
	if err != nil {
		return err
	}
	var manifest skillManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	if manifest.ID == "" || strings.ContainsAny(manifest.ID, "\r\n") || len(manifest.Paths) == 0 {
		return errors.New("invalid skills snapshot manifest")
	}
	// The manifest is workspace content, never authority for arbitrary deletion.
	for _, target := range manifest.Paths {
		parts := strings.Split(target, "/")
		if len(parts) != 3 || parts[1] != "skills" || !safeSkillPath(parts[2]) || (parts[0] != ".github" && parts[0] != ".claude" && parts[0] != ".agents") {
			return fmt.Errorf("unsafe skill snapshot path %q", target)
		}
	}
	manifest.Git = isGitWorkspace(context.Background(), s.workspace)
	s.manifest = manifest
	return s.cleanup()
}

func (s *skillSnapshot) cleanup() error {
	if s.manifest.ID == "" {
		return nil
	}
	if s.manifest.Git {
		if err := s.refuseTracked(context.Background(), append([]string{skillStateDir}, s.manifest.Paths...)...); err != nil {
			return err
		}
	}
	for _, target := range s.manifest.Paths {
		if err := skillMkdirAll(s.root, path.Dir(target)); err != nil {
			return err
		}
		if err := s.root.RemoveAll(target); err != nil {
			return err
		}
	}
	if s.manifest.Git {
		if err := gitexclude.RemoveScoped(context.Background(), s.workspace, s.manifest.ID); err != nil {
			return err
		}
	}
	if err := s.root.Remove(skillManifestPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	s.manifest = skillManifest{}
	return nil
}

func (s *skillSnapshot) Close() error {
	if s == nil {
		return nil
	}
	var err error
	if s.held != nil {
		err = s.cleanup()
	}
	return errors.Join(err, s.held.Release(), s.root.Close())
}

func isGitWorkspace(ctx context.Context, workspace string) bool {
	out, err := skillGit(ctx, workspace, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func skillGit(ctx context.Context, workspace string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workspace
	return cmd.Output()
}
