package v1alpha1

import (
	"errors"
	"testing"
)

func TestValidateOutboxPathRejectsEmptyAndEscapingEntries(t *testing.T) {
	for _, path := range []string{"", "   ", "..", "../outside", "reports/../../outside", "/etc/passwd", `C:\secrets`, `\rooted`} {
		if err := ValidateOutboxPath(path); !errors.Is(err, ErrPathEscape) {
			t.Errorf("ValidateOutboxPath(%q) = %v; want ErrPathEscape", path, err)
		}
	}
}

func TestValidateOutboxPathAcceptsContainedEntries(t *testing.T) {
	for _, path := range []string{"report.md", "reports", "reports/summary.json", "./reports/summary.json", "a/b/../c"} {
		if err := ValidateOutboxPath(path); err != nil {
			t.Errorf("ValidateOutboxPath(%q) = %v; want nil", path, err)
		}
	}
}

func TestValidateOutboxMirrorRootRejectsRelativeRoots(t *testing.T) {
	for _, root := range []string{"", "  ", "reports", "./reports", "../reports", "~reports"} {
		if err := ValidateOutboxMirrorRoot(root); !errors.Is(err, ErrInvalidOutboxMirrorRoot) {
			t.Errorf("ValidateOutboxMirrorRoot(%q) = %v; want ErrInvalidOutboxMirrorRoot", root, err)
		}
	}
}

func TestValidateOutboxMirrorRootAcceptsAbsoluteAndHomeRelativeRoots(t *testing.T) {
	for _, root := range []string{"~/", "~/goobers/outbox", "/var/lib/goobers", `C:\\goobers\\outbox`} {
		if err := ValidateOutboxMirrorRoot(root); err != nil {
			t.Errorf("ValidateOutboxMirrorRoot(%q) = %v; want nil", root, err)
		}
	}
}
