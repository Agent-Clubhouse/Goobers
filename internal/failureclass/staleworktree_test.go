package failureclass

import "testing"

func TestIsStaleManagedWorktreePath(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name    string
		message string
		want    bool
	}{
		{
			name:    "golangci-lint cached Windows worktree path",
			message: `failed to parse file: open C:\runs\wt-477de6f81fdc8e7507549359\api\v1alpha1\zz_generated.deepcopy.go: The system cannot find the path specified.`,
			want:    true,
		},
		{
			name:    "missing managed worktree path on Unix",
			message: `open /var/lib/goobers/wt-477de6f81fdc8e7507549359/api/v1alpha1/generated.go: no such file or directory`,
			want:    true,
		},
		{
			name:    "ordinary missing source file",
			message: `open C:\repo\api\v1alpha1\zz_generated.deepcopy.go: The system cannot find the path specified.`,
			want:    false,
		},
		{
			name:    "managed worktree path with content error",
			message: `C:\runs\wt-477de6f81fdc8e7507549359\api\v1alpha1\generated.go:12: undefined: Widget`,
			want:    false,
		},
		{
			name:    "lookalike worktree directory",
			message: `open C:\runs\wt-not-a-managed-hash\generated.go: The system cannot find the path specified.`,
			want:    false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := IsStaleManagedWorktreePath(testCase.message); got != testCase.want {
				t.Fatalf("IsStaleManagedWorktreePath(%q) = %t, want %t", testCase.message, got, testCase.want)
			}
		})
	}
}
