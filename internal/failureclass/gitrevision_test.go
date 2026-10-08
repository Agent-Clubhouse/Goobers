package failureclass

import "testing"

func TestIsUnrunnableGitRevisionFailure(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name    string
		message string
		want    bool
	}{
		{
			name:    "diff against a ref the workcopy lacks",
			message: "command exited 128; stderr: fatal: ambiguous argument 'origin/main...HEAD': unknown revision or path not in the working tree.\nUse '--' to separate paths from revisions, like this:",
			want:    true,
		},
		{
			name:    "selected failure diagnostic",
			message: "command exited 128; failure: fatal: ambiguous argument 'origin/main...HEAD': unknown revision or path not in the working tree.",
			want:    true,
		},
		{
			name:    "plumbing bad revision",
			message: "command exited 128; stderr: fatal: bad revision 'origin/main'",
			want:    true,
		},
		{
			name:    "wrapper exit status",
			message: "command exited 2; stderr: fatal: ambiguous argument 'origin/main...HEAD': unknown revision or path not in the working tree.",
			want:    false,
		},
		{
			name:    "exit status prefix is exact",
			message: "command exited 1280; stderr: fatal: bad revision 'origin/main'",
			want:    false,
		},
		{
			name:    "ambiguity between a revision and a path",
			message: "command exited 128; stderr: fatal: ambiguous argument 'main': both revision and filename",
			want:    false,
		},
		{
			name:    "other git fatal",
			message: "command exited 128; stderr: fatal: refusing to merge unrelated histories",
			want:    false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := IsUnrunnableGitRevisionFailure(testCase.message); got != testCase.want {
				t.Fatalf("IsUnrunnableGitRevisionFailure(%q) = %t, want %t", testCase.message, got, testCase.want)
			}
		})
	}
}
