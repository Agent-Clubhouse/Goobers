package v1alpha1

import (
	"errors"
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/pathutil"
)

// ErrInvalidOutboxMirrorRoot marks a configured outbox mirror root that cannot
// resolve to an absolute local directory. The mirror root is expanded on the
// runner host, so a relative root has no stable meaning there; rejecting it
// during semantic validation keeps the failure at config load instead of
// converting completed stage work into an export failure.
var ErrInvalidOutboxMirrorRoot = errors.New("invalid outbox mirror root")

// ValidateOutboxPath reports whether a declared task outbox entry is a usable
// workspace-relative path: non-empty, not absolute or volume-bound, and not
// escaping the workspace via "..". It is the lexical half of the containment
// rule ResolveContainedPath enforces (symlink-aware) at export time, so a
// declaration the runtime boundary would reject is caught before the stage
// ever runs.
func ValidateOutboxPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%w: empty path", ErrPathEscape)
	}
	_, err := containedPath("", path)
	return err
}

// ValidateOutboxMirrorRoot reports whether a configured outbox mirror root is
// absolute or home-relative ("~/"). The rooted test is deliberately
// platform-independent — a POSIX root stays valid when the config is validated
// on a Windows host and vice versa — so validation never rejects a document
// merely for being read on the other platform; the runner applies its own
// host-local expansion on top.
func ValidateOutboxMirrorRoot(root string) error {
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("%w: empty root", ErrInvalidOutboxMirrorRoot)
	}
	if strings.HasPrefix(root, "~/") || pathutil.IsRootedOrVolumeBound(root) {
		return nil
	}
	return fmt.Errorf("%w: %q must be absolute or start with ~/", ErrInvalidOutboxMirrorRoot, root)
}
