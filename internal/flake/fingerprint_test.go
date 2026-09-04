package flake

import "testing"

// #3990 and #4000 are the same defect: `./internal/worktree`
// TestApplyBundleArmsAndBundleRunBranch losing its t.TempDir cleanup to a
// background git process. The ledger filed it twice because the failure names
// the directory it could not remove, and every run's directory has a fresh
// random suffix and a fresh mirror key — volatile values as surely as an
// address or a duration, and the fingerprint must not split a defect on them.
func TestNormalizeSignatureFoldsTempDirectoryIdentities(t *testing.T) {
	first := "testing.go:1464: TempDir RemoveAll cleanup: unlinkat " +
		"/tmp/TestApplyBundleArmsAndBundleRunBranch833401532/002/fabc63ec11b7f072/repo.git: directory not empty"
	second := "testing.go:1464: TempDir RemoveAll cleanup: unlinkat " +
		"/tmp/TestApplyBundleArmsAndBundleRunBranch1845522470/002/1e9b13250cedcfdf/repo.git: directory not empty"

	firstSignature := NormalizeSignature(first)
	secondSignature := NormalizeSignature(second)
	if firstSignature != secondSignature {
		t.Fatalf("NormalizeSignature split one defect into two identities:\n %q\n %q", firstSignature, secondSignature)
	}
	const pkg, test = "./internal/worktree", "TestApplyBundleArmsAndBundleRunBranch"
	if Fingerprint(pkg, test, firstSignature) != Fingerprint(pkg, test, secondSignature) {
		t.Fatal("Fingerprint differs for two occurrences of the same defect")
	}
	if got, want := firstSignature,
		"TempDir RemoveAll cleanup: unlinkat /tmp/TestApplyBundleArmsAndBundleRunBranch<rand>/002/<hash>/repo.git: directory not empty"; got != want {
		t.Fatalf("NormalizeSignature() = %q, want %q", got, want)
	}
}

// Folding the volatile parts of a path must not fold the parts that identify
// which failure it was: a different directory, file, or message in the same
// test is still a different defect.
func TestNormalizeSignatureKeepsDistinctPathFailuresApart(t *testing.T) {
	const prefix = "testing.go:1464: TempDir RemoveAll cleanup: unlinkat /tmp/TestThing723409821/002/"
	mirror := NormalizeSignature(prefix + "aabbccdd11223344/repo.git: directory not empty")
	runs := NormalizeSignature(prefix + "aabbccdd11223344/runs/stage: directory not empty")
	if mirror == runs {
		t.Fatalf("two different undeletable paths share the signature %q", mirror)
	}
	other := NormalizeSignature("worktree.go:210: Create: exit status 128")
	if other == mirror {
		t.Fatal("an unrelated failure shares the cleanup signature")
	}
}

// Digits and hashes that are not path segments carry meaning — an exit code,
// an expected count, a SHA an assertion names — and must survive.
func TestNormalizeSignatureKeepsNonPathValues(t *testing.T) {
	got := NormalizeSignature("bundle_test.go:120: ApplyBundle (stale) = keep at 4f8c2b1d9e0a7c6b5a4938271605f4e3d2c1b0a9, want create")
	want := "ApplyBundle (stale) = keep at 4f8c2b1d9e0a7c6b5a4938271605f4e3d2c1b0a9, want create"
	if got != want {
		t.Fatalf("NormalizeSignature() = %q, want %q", got, want)
	}
}

// The Go test runner echoes `-test.shuffle <seed>` on a failing package, and
// for a package-level failure that echo can be the only line left after
// boilerplate. The seed changes every run, so keeping it splits one recurring
// failure into one issue per run (#4221).
func TestNormalizeSignatureFoldsRunnerFlagValues(t *testing.T) {
	tests := []struct {
		name  string
		first string
		other string
		same  bool
		want  string
	}{
		{
			name:  "shuffle seed alone",
			first: "-test.shuffle 1788254672532515140",
			other: "-test.shuffle 1788173513904988007",
			same:  true,
			want:  "-test.shuffle <value>",
		},
		{
			name:  "shuffle seed alongside an assertion",
			first: "runner_test.go:42: Resume() = 3, want 4\n-test.shuffle 1788254672532515140",
			other: "runner_test.go:42: Resume() = 3, want 4\n-test.shuffle 1788083830159618170",
			same:  true,
			want:  "Resume() = 3, want 4 | -test.shuffle <value>",
		},
		{
			name:  "equals form and numeric limits",
			first: "-test.shuffle=1788254672532515140 -test.count=17",
			other: "-test.shuffle=1788083830159618170 -test.count=3",
			same:  true,
			want:  "-test.shuffle=<value> -test.count=<value>",
		},
		{
			name:  "distinct assertions stay distinct",
			first: "runner_test.go:42: Resume() = 3, want 4\n-test.shuffle 1788254672532515140",
			other: "runner_test.go:42: Resume() = 9, want 4\n-test.shuffle 1788254672532515140",
			same:  false,
			want:  "Resume() = 3, want 4 | -test.shuffle <value>",
		},
		{
			name:  "non-numeric flag values are meaning, not noise",
			first: "-test.run TestResume",
			other: "-test.run TestRestart",
			same:  false,
			want:  "-test.run TestResume",
		},
	}
	const pkg, test = "./cmd/goobers", ""
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			first := NormalizeSignature(testCase.first)
			other := NormalizeSignature(testCase.other)
			if first != testCase.want {
				t.Fatalf("NormalizeSignature() = %q, want %q", first, testCase.want)
			}
			got := Fingerprint(pkg, test, first) == Fingerprint(pkg, test, other)
			if got != testCase.same {
				t.Fatalf("fingerprints equal = %t, want %t (%q vs %q)", got, testCase.same, first, other)
			}
		})
	}
}
