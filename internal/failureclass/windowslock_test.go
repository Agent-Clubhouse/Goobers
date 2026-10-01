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
			name:    "go rename access denied (Windows AV lock)",
			message: `rename C:\work\.goobers\.agent-toolkit-install-123 C:\work\.goobers\agent-toolkit: Access is denied`,
			want:    true,
		},
		{
			name:    "go rename access denied with source spaces",
			message: `rename C:\Users\Jane Doe\source C:\work\dest: Access is denied`,
			want:    true,
		},
		{
			name:    "go rename access denied with destination spaces",
			message: `rename C:\work\source C:\Users\Jane Doe\dest: Access is denied`,
			want:    true,
		},
		{
			name:    "go rename access denied with source and destination spaces",
			message: `rename C:\Users\Jane Doe\source C:\Users\Jane Doe\dest: Access is denied`,
			want:    true,
		},
		{
			name:    "go rename access denied selected segment before assertion evidence",
			message: `rename C:\Users\Jane Doe\source C:\Users\Jane Doe\dest: Access is denied | credential check failed: access is denied`,
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
			name:    "genuine HTTP access denial is not a lock signature",
			message: `PUT https://api.example.com/v1/widgets: 403 access is denied for this credential`,
			want:    false,
		},
		{
			name:    "unrelated rename chatter does not taint access denial",
			message: `--- FAIL: TestRenameUnauthorized (0.10s): PUT https://api.example.com/v1/widgets: 403 access is denied for this credential`,
			want:    false,
		},
		{
			name:    "logged rename paths do not taint later access denial",
			message: `cleanup logged rename C:\work\old C:\work\new; PUT https://api.example.com/v1/widgets: access is denied for this credential`,
			want:    false,
		},
		{
			name:    "logged rename paths do not taint later credential denial",
			message: `cleanup logged rename C:\work\old C:\work\new; credential check failed: access is denied`,
			want:    false,
		},
		{
			name:    "logged rename paths do not span executor evidence delimiter",
			message: `cleanup logged rename C:\work\old C:\work\new | credential check failed: access is denied`,
			want:    false,
		},
		{
			name:    "logged rename paths do not absorb same-segment prose",
			message: `cleanup logged rename C:\work\old C:\work\new credential check failed: access is denied`,
			want:    false,
		},
		{
			name:    "boundary rename paths do not absorb same-segment prose",
			message: `rename C:\work\old C:\work\new credential check failed: access is denied`,
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
