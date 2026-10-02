package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
)

// Query ancestors too: a tracked file or symlink may be absent from the working
// tree (including via skip-worktree), so filesystem checks alone cannot protect
// it from being recreated as a snapshot directory. Ignore unrelated descendants
// of those ancestors, such as another repository-owned skill package.
func (s *skillSnapshot) refuseTracked(ctx context.Context, targets ...string) error {
	args := []string{"--literal-pathspecs", "ls-files", "-z", "--"}
	for _, target := range targets {
		for current := target; current != "."; current = path.Dir(current) {
			args = append(args, current)
		}
	}
	data, err := skillGit(ctx, s.workspace, args...)
	if err != nil {
		return err
	}
	for _, tracked := range strings.Split(string(data), "\x00") {
		if tracked == "" {
			continue
		}
		for _, target := range targets {
			if tracked == target || strings.HasPrefix(tracked, target+"/") || strings.HasPrefix(target, tracked+"/") {
				return fmt.Errorf("skill snapshot path %q collides with tracked repository path %q", target, tracked)
			}
		}
	}
	return nil
}

func (s *skillSnapshot) writeManifest(manifest skillManifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	// Recovery has removed any previous manifest. Exclusive creation also
	// refuses a dangling symlink, including one introduced since recovery.
	file, err := s.root.OpenFile(skillManifestPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	s.manifest = manifest
	_, err = file.Write(data)
	return errors.Join(err, file.Close())
}
