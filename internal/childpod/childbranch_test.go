package childpod

import "testing"

func TestChildContractBindsStaticBranchWithoutGrantingParentRole(t *testing.T) {
	r := requestFixture()
	r.ChildBranch = 2
	c, _, err := makeContract(t.Context(), r)
	if err != nil || c.ChildBranch != 2 {
		t.Fatal(c, err)
	}
	for _, branch := range []int{-1, 129} {
		changed := c
		changed.ChildBranch = branch
		if changed.Validate() == nil {
			t.Fatal("invalid child branch accepted", branch)
		}
	}
	changed := c
	changed.ParentBranch = 2
	if changed.Validate() == nil {
		t.Fatal("child acquired parent branch role")
	}
	retained := retainedFixture()
	retained.Input.Attempt = r.Attempt
	if err := verifyRetainedContract(r, retained, c); err != nil {
		t.Fatal("matching child branch refused", err)
	}
	r.ChildBranch = 1
	if verifyRetainedContract(r, retained, c) == nil {
		t.Fatal("recovery rebound child branch")
	}
}
