package triggerqueue

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/sessioning"
)

func sessionRepairFixture() *sessioning.PRRepairTarget {
	return &sessioning.PRRepairTarget{SourceBindingID: "code", Repository: sessioning.RepairRepository{Provider: "github", Owner: "org", Name: "repo"}, RepositorySourceID: "100", ID: "12", SourceID: "900", ExpectedHeadSHA: strings.Repeat("a", 40)}
}
func TestSessionRepairSelectionDurableQueueContextAndTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openTestStore(t, path)
	conversation := createSessionTest(t, s, "create")
	request := sessioning.MessageRequest{Text: "Repair the selected PR", RepairTarget: sessionRepairFixture()}
	original := *request.RepairTarget
	accepted, err := s.SubmitSessionInput(t.Context(), sessionCommand("selection"), conversation.ID, request, []byte(`{}`), childTestTime)
	if err != nil || accepted.Message == nil || *accepted.Message.RepairTarget != original {
		t.Fatal(accepted, err)
	}
	request.RepairTarget.ExpectedHeadSHA = strings.Repeat("b", 40)
	other := openTestStore(t, path)
	turn, err := other.SessionTurn(t.Context(), accepted.AcceptanceID)
	if err != nil || *turn.Message.RepairTarget != original {
		t.Fatal("selection not retained", turn, err)
	}
	input, err := other.SessionInputs(t.Context(), accepted.AcceptanceID)
	if err != nil || *input.Messages[0].RepairTarget != original {
		t.Fatal(input, err)
	}
	if input.Start.MessageDigest == sessioning.Digest([]byte(request.Text)) {
		t.Fatal("selection excluded from input digest")
	}
	page, err := other.SessionMessages(t.Context(), "gaggle", conversation.ID, 0, 50)
	if err != nil || *page.Items[0].RepairTarget != original {
		t.Fatal(page, err)
	}
	corrupt, _ := json.Marshal(request.RepairTarget)
	if _, err = other.db.Exec(`UPDATE interactive_messages SET repair_target=? WHERE id=?`, corrupt, accepted.Message.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = other.SessionTurn(t.Context(), accepted.AcceptanceID); err == nil {
		t.Fatal("changed message selection accepted by queue")
	}
	if _, err = other.SessionInputs(t.Context(), accepted.AcceptanceID); err == nil {
		t.Fatal("frozen context bypassed changed custody")
	}
}
func TestSessionRepairSelectionRefusesMalformedOrAgentMetadata(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	conversation := createSessionTest(t, s, "create")
	target := sessionRepairFixture()
	target.SourceID = "invalid"
	if _, err := s.SubmitSessionInput(t.Context(), sessionCommand("bad"), conversation.ID, sessioning.MessageRequest{Text: "repair", RepairTarget: target}, []byte(`{}`), childTestTime); err == nil {
		t.Fatal("invalid target admitted")
	}
	accepted, err := s.SubmitSessionInput(t.Context(), sessionCommand("good"), conversation.ID, sessioning.MessageRequest{Text: "repair", RepairTarget: sessionRepairFixture()}, []byte(`{}`), childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE interactive_messages SET actor_kind='agent',actor='null' WHERE id=?`, accepted.Message.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SessionMessages(t.Context(), "gaggle", conversation.ID, 0, 50); err == nil {
		t.Fatal("agent-owned human selection exposed")
	}
}
