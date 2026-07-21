package providers

import "testing"

func TestNormalizeBranchNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"empty defaults", "", DefaultBranchNamespace},
		{"already trailing slash", "acme/", "acme/"},
		{"adds trailing slash", "acme", "acme/"},
		{"nested with slash", "acme/team/", "acme/team/"},
		{"nested without slash", "acme/team", "acme/team/"},
		{"default value is idempotent", "goobers/", "goobers/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeBranchNamespace(tc.in); got != tc.want {
				t.Errorf("NormalizeBranchNamespace(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBranchNameIn(t *testing.T) {
	// BranchName is the DefaultBranchNamespace specialization of BranchNameIn:
	// they must agree so callers with no configured namespace get identical
	// output (the seam-of-truth invariant behind #965).
	if got, want := BranchName("implementation", "run1"), "goobers/implementation/run1"; got != want {
		t.Errorf("BranchName = %q, want %q", got, want)
	}
	if got, want := BranchNameIn("", "implementation", "run1"), BranchName("implementation", "run1"); got != want {
		t.Errorf("BranchNameIn(\"\", ...) = %q, want it equal to BranchName = %q", got, want)
	}

	// A gaggle-configured namespace flows through, with or without a trailing
	// slash, keeping the "<namespace><workflow>/<run>" shape.
	for _, tc := range []struct {
		ns, want string
	}{
		{"acme/", "acme/implementation/run1"},
		{"acme", "acme/implementation/run1"},
		{"acme/team/", "acme/team/implementation/run1"},
	} {
		if got := BranchNameIn(tc.ns, "implementation", "run1"); got != tc.want {
			t.Errorf("BranchNameIn(%q, ...) = %q, want %q", tc.ns, got, tc.want)
		}
	}
}
