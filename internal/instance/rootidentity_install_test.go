package instance

import "testing"

func TestInstallRootIdentitySharesAuthorityWithoutMinting(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	for range 2 {
		root := t.TempDir()
		if err := InstallRootIdentity(t.Context(), root, id); err != nil {
			t.Fatal(err)
		}
		if got, err := ReadRootIdentity(root); err != nil || got != id {
			t.Fatalf("identity = %q, %v", got, err)
		}
		if err := InstallRootIdentity(t.Context(), root, "fedcba9876543210fedcba9876543210"); err == nil {
			t.Fatal("authoritative identity was replaced")
		}
	}
}
