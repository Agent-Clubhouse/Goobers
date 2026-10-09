package childpod

import (
	"encoding/json"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func parentContractFixture() Contract {
	r := requestFixture()
	r.Identity.Child = nil
	id := journal.StageAttemptID(r.Identity.RunID, 2, "plan", 11)
	return Contract{Version: 1, Identity: r.Identity, Stage: "plan", Attempt: 1, PodAttempt: 11, ParentBranch: 2, StartedAt: r.StartedAt, Ceiling: r.Ceiling, ParentOrigin: &apiv1.ChildWorkflowOrigin{StageOccurrence: id, AttemptID: id}}
}

func TestParentContractBindsPhysicalAttemptAndRole(t *testing.T) {
	c := parentContractFixture()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeContract(data, journal.Digest(data)); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Contract){
		"wrong branch":           func(c *Contract) { c.ParentBranch++ },
		"wrong physical attempt": func(c *Contract) { c.PodAttempt++ },
		"wrong stage":            func(c *Contract) { c.Stage = "foreign" },
		"missing origin":         func(c *Contract) { c.ParentOrigin = nil },
		"child owner":            func(c *Contract) { c.Identity.Child = requestFixture().Identity.Child },
		"unbound occurrence":     func(c *Contract) { c.ParentOrigin.StageOccurrence = "invented" },
		"unbound attempt":        func(c *Contract) { c.ParentOrigin.AttemptID = "invented" },
		"wrong source":           func(c *Contract) { c.Identity.WorkflowDigest = "short" },
		"provider delegation":    func(c *Contract) { c.Ceiling.AllowPublication = true },
	} {
		t.Run(name, func(t *testing.T) {
			changed := c
			origin := *c.ParentOrigin
			changed.ParentOrigin = &origin
			change(&changed)
			if changed.Validate() == nil {
				t.Fatal("invalid parent custody accepted")
			}
		})
	}
}
