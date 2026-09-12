package recovery

import "testing"

func TestRemoteRecoveryBaseRef(t *testing.T) {
	for _, test := range []struct {
		local string
		want  string
	}{
		{local: "main", want: "refs/heads/main"},
		{local: "master", want: "refs/heads/master"},
		{local: "release/2026.09", want: "refs/heads/release/2026.09"},
		{local: "refs/heads/master", want: "refs/heads/master"},
		{local: "refs/remotes/mirror/master", want: "refs/heads/master"},
		{local: "refs/remotes/mirror/release/2026.09", want: "refs/heads/release/2026.09"},
		{local: "refs/tags/v1.0.0", want: "refs/tags/v1.0.0"},
		{local: "0123456789012345678901234567890123456789", want: "0123456789012345678901234567890123456789"},
	} {
		if got := remoteRecoveryBaseRef(test.local); got != test.want {
			t.Errorf("remoteRecoveryBaseRef(%q) = %q, want %q", test.local, got, test.want)
		}
	}
}
