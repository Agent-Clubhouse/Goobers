package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Inspect the index once for the entire target batch, without Git pathspec
// filtering: its case-sensitive matching can hide absent tracked ancestors or
// descendants on case-insensitive filesystems. Conservatively protect case-fold
// aliases on every platform, including Unicode aliases, independently of Git's
// core.ignoreCase setting. Unrelated sibling packages remain permitted.
func (s *skillSnapshot) refuseTracked(ctx context.Context, targets ...string) error {
	data, err := skillGit(ctx, s.workspace, "ls-files", "-z")
	if err != nil {
		return err
	}
	for _, tracked := range strings.Split(string(data), "\x00") {
		if tracked == "" {
			continue
		}
		for _, target := range targets {
			if skillPathsOverlap(tracked, target) {
				return fmt.Errorf("skill snapshot path %q collides with tracked repository path %q", target, tracked)
			}
		}
	}
	return nil
}

func skillPathsOverlap(first, second string) bool {
	left, right := strings.Split(first, "/"), strings.Split(second, "/")
	for i := range min(len(left), len(right)) {
		if !strings.EqualFold(left[i], right[i]) {
			return false
		}
	}
	return true
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
