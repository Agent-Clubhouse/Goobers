package goobernetes

import (
	"os"
	"testing"
	"testing/fstest"
)

// TestScanTextForKubectlExecCatchesPlainAndFlagged proves the transcript
// scanner (S7's observer, D3's "the procedure transcript itself is an
// observer") catches both the bare form and a flagged invocation
// ("kubectl -n gaggle exec ...").
func TestScanTextForKubectlExecCatchesPlainAndFlagged(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"clean transcript", "goobers run smoke-workflow\ngoobers trace run-1\n", 0},
		{"bare kubectl exec", "kubectl exec -it daemon-pod -- sh\n", 1},
		{"flagged kubectl exec", "kubectl -n goobers-system exec -it daemon-pod -c app -- sh\n", 1},
		{"kubectl debug is also pod-exec-equivalent", "kubectl debug node/node-a -it --image=busybox\n", 1},
		{"kubectl get is fine", "kubectl get pods -n goobers-system\n", 0},
		{"kubectl execute-something is not exec", "kubectl execute-plan --dry-run\n", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanTextForKubectlExec(tc.text)
			if len(got) != tc.want {
				t.Fatalf("ScanTextForKubectlExec(%q) = %v, want %d violation(s)", tc.text, got, tc.want)
			}
		})
	}
}

// TestScanGoStringLiteralsForKubectlExecCatchesAViolation proves the guard
// mechanism works at all, using a synthetic in-memory file — this is the
// fixture the guard's own correctness rests on, independent of whether this
// package's real source currently contains a violation.
func TestScanGoStringLiteralsForKubectlExecCatchesAViolation(t *testing.T) {
	dirty := "package fixture\n\nconst cmd = \"kubectl exec -it pod -- sh\"\n"
	fsys := fstest.MapFS{
		"dirty.go": &fstest.MapFile{Data: []byte(dirty)},
	}
	violations, err := ScanGoStringLiteralsForKubectlExec(fsys, ".")
	if err != nil {
		t.Fatalf("ScanGoStringLiteralsForKubectlExec: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %v, want exactly 1", violations)
	}
}

// TestScanGoStringLiteralsForKubectlExecIgnoresComments proves the AST-based
// guard scans string literals, not doc comments — a comment that discusses
// the rule in prose (exactly what this package's own doc comments do) must
// never trip the guard on itself.
func TestScanGoStringLiteralsForKubectlExecIgnoresComments(t *testing.T) {
	clean := "package fixture\n\n// This package must never run kubectl exec anywhere.\nconst cmd = \"kubectl get pods\"\n"
	fsys := fstest.MapFS{"clean.go": &fstest.MapFile{Data: []byte(clean)}}
	violations, err := ScanGoStringLiteralsForKubectlExec(fsys, ".")
	if err != nil {
		t.Fatalf("ScanGoStringLiteralsForKubectlExec: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none (comments are not scanned)", violations)
	}
}

// TestScanGoStringLiteralsForKubectlExecIgnoresTestFiles proves _test.go
// files are excluded — this guard's own fixture tests above deliberately
// construct violating strings to prove detection works, and must not trip
// the guard when it scans itself.
func TestScanGoStringLiteralsForKubectlExecIgnoresTestFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"thing_test.go": &fstest.MapFile{Data: []byte("package fixture\n\nconst cmd = \"kubectl exec -it pod -- sh\"\n")},
	}
	violations, err := ScanGoStringLiteralsForKubectlExec(fsys, ".")
	if err != nil {
		t.Fatalf("ScanGoStringLiteralsForKubectlExec: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none (_test.go files are excluded)", violations)
	}
}

// TestHarnessOwnSourceContainsNoKubectlExec is the ACTUAL §5 rule 1 / D3
// guard: this package's own non-test source (its "procedure/commands") must
// never contain kubectl exec (or kubectl debug) as a literal string. This is
// what the #3517 task asks for: "a check that scans the harness's own
// procedure/commands and FAILS if kubectl exec... appears."
func TestHarnessOwnSourceContainsNoKubectlExec(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	violations, err := ScanGoStringLiteralsForKubectlExec(os.DirFS(wd), ".")
	if err != nil {
		t.Fatalf("ScanGoStringLiteralsForKubectlExec: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("goobernetes harness source contains kubectl exec/debug (goobernetes-smoke.md §5 rule 1: a filed defect): %v", violations)
	}
}
