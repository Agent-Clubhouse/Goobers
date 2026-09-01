package failureclass

import "testing"

func TestIsDependencyTransportDenial(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name    string
		message string
		want    bool
	}{
		{
			name:    "npm tarball 403 from a private mirror",
			message: `npm error 403 403 Forbidden - GET https://ms-feed-12.pkgs.visualstudio.com/1es-public/_packaging/npm-public/npm/registry/three/-/three-0.185.1.tgz`,
			want:    true,
		},
		{
			name:    "module proxy 403",
			message: `go: example.com/mod@v1.2.3: reading https://proxy.golang.org/example.com/mod/@v/v1.2.3.zip: 403 Forbidden`,
			want:    true,
		},
		{
			name:    "npm registry timeout",
			message: `npm error network request to https://registry.npmjs.org/@playwright%2ftest failed, reason: connect ETIMEDOUT`,
			want:    true,
		},
		{
			name:    "application 403 without a dependency fetch",
			message: `--- FAIL: TestForbidden: want 200, got 403 Forbidden from https://api.example.com/v1/widgets`,
			want:    false,
		},
		{
			name:    "dependency fetch without a transport denial",
			message: `go: downloading example.com/mod v1.2.3: checksum mismatch against sum.golang.org`,
			want:    false,
		},
		{
			name:    "npm diagnostic without a transport denial",
			message: `npm error code ELIFECYCLE`,
			want:    false,
		},
		{
			name:    "wrapper trailer names no cause",
			message: `make: *** [Makefile:352: ci] Error 1`,
			want:    false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := IsDependencyTransportDenial(testCase.message); got != testCase.want {
				t.Fatalf("IsDependencyTransportDenial(%q) = %t, want %t", testCase.message, got, testCase.want)
			}
		})
	}
}

func TestIsDependencyTransportDenialIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	if !IsDependencyTransportDenial(`NPM ERROR 403 403 FORBIDDEN - GET https://feed.example.com/three.tgz`) {
		t.Fatal("uppercase denial was not recognized")
	}
}
