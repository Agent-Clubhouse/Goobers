package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Inspect the index once for the entire target batch, without Git pathspec
// filtering: its case-sensitive matching can hide absent tracked ancestors or
// descendants on case-insensitive filesystems. Conservatively protect case-fold
// aliases on every platform, including canonical Unicode and ignorable-character
// aliases, independently of Git's core.ignoreCase setting. Unrelated sibling
// packages remain permitted.
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
		if !strings.EqualFold(skillFilesystemName(left[i]), skillFilesystemName(right[i])) {
			return false
		}
	}
	return true
}

// Use a conservative cross-platform equivalence rule rather than relying on the
// host filesystem: some filesystems normalize Unicode and ignore these code
// points. New snapshot names reject them; existing index entries must still be
// compared without them so they cannot evade collision protection.
func skillFilesystemName(name string) string {
	return norm.NFD.String(strings.Map(func(r rune) rune {
		if skillIgnorableRune(r) {
			return -1
		}
		return r
	}, name))
}

func skillIgnorableRune(r rune) bool {
	return unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) || unicode.Is(unicode.Variation_Selector, r)
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
