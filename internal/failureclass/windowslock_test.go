package failureclass

import "testing"

func TestIsWindowsSharingViolation(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name    string
		message string
		want    bool
	}{
		{
			name:    "git worktree remove sharing violation",
			message: `fatal: unable to unlink 'src/main.go': Permission denied`,
			want:    true,
		},
		{
			name:    "os.RemoveAll native syscall lock",
			message: `remove C:\worktrees\r1\main.go: The process cannot access the file because it is being used by another process.`,
			want:    true,
		},
		{
			name:    "raw ERROR_SHARING_VIOLATION text",
			message: `open C:\repo\.git\index: sharing violation`,
			want:    true,
		},
		{
			name:    "node EBUSY on a locked directory",
			message: `Error: EBUSY: resource busy or locked, rmdir 'C:\repo\node_modules\.tmp'`,
			want:    true,
		},
		{
			name:    "node EPERM unlink (Windows AV lock)",
			message: `Error: EPERM: operation not permitted, unlink 'C:\repo\node_modules\pkg\index.js'`,
			want:    true,
		},
		{
			name:    "node EPERM rename (Windows AV lock)",
			message: `Error: EPERM: operation not permitted, rename 'C:\repo\dist\old.js' -> 'C:\repo\dist\new.js'`,
			want:    true,
		},
		{
			name:    "generic used-by-another-process phrasing",
			message: `cannot remove 'build/output.bin': used by another process`,
			want:    true,
		},
		{
			name:    "case insensitive",
			message: `FATAL: SHARING VIOLATION WHILE OPENING FILE`,
			want:    true,
		},
		{
			name:    "genuine EPERM on open is not a lock signature",
			message: `Error: EPERM: operation not permitted, open '/etc/shadow'`,
			want:    false,
		},
		{
			name:    "genuine linux permission denied on a protected mount",
			message: `mkdir /var/lib/protected: permission denied`,
			want:    false,
		},
		{
			name:    "genuine authorization denial with no lock context",
			message: `ssh: handshake failed: permission denied (publickey)`,
			want:    false,
		},
		{
			name:    "unrelated nonzero exit",
			message: `exit status 1`,
			want:    false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := IsWindowsSharingViolation(testCase.message); got != testCase.want {
				t.Fatalf("IsWindowsSharingViolation(%q) = %t, want %t", testCase.message, got, testCase.want)
			}
		})
	}
}
