//go:build linux

package secfile

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestIsReadOnlyTmpfs(t *testing.T) {
	statfsError := errors.New("statfs failed")
	tests := []struct {
		name   string
		fsType int64
		flags  int64
		err    error
		want   bool
	}{
		{name: "read-only tmpfs", fsType: unix.TMPFS_MAGIC, flags: unix.ST_RDONLY, want: true},
		{name: "writable tmpfs", fsType: unix.TMPFS_MAGIC},
		{name: "read-only other filesystem", fsType: unix.EXT4_SUPER_MAGIC, flags: unix.ST_RDONLY},
		{name: "statfs failure", err: statfsError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isReadOnlyTmpfsWith("/secret", func(path string, fs *unix.Statfs_t) error {
				if path != "/secret" {
					t.Errorf("statfs path = %q, want /secret", path)
				}
				fs.Type = tt.fsType
				fs.Flags = tt.flags
				return tt.err
			})
			if got != tt.want {
				t.Errorf("isReadOnlyTmpfsWith() = %t, want %t", got, tt.want)
			}
		})
	}
}
