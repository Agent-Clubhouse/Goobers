package sessioning

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func repairTargetFixture() *PRRepairTarget {
	return &PRRepairTarget{SourceBindingID: "code", Repository: RepairRepository{Provider: "github", Owner: "org", Name: "repo"}, RepositorySourceID: "100", ID: "12", SourceID: "900", ExpectedHeadSHA: strings.Repeat("a", 40)}
}
func repairExecutionFixture() ExecutionInputs {
	digest := "sha256:" + strings.Repeat("b", 64)
	message := Message{ID: "message-one", SessionID: "session-one", Sequence: 1, ActorKind: "human", Actor: &Actor{Issuer: "https://identity", Subject: "alice"}, Text: "Repair this PR", CreatedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), TurnID: "turn-one", RepairTarget: repairTargetFixture()}
	start := StartEnvelope{Kind: StartKind, Gaggle: "gaggle", SessionID: message.SessionID, TurnID: message.TurnID, MessageID: message.ID, MessageDigest: MessageDigest(message.Text, message.RepairTarget), AuthorityDigest: digest, Profile: Profile{Goober: "planner", ConfigGeneration: digest, GooberDigest: digest}}
	return ExecutionInputs{Version: 1, AcceptanceID: "trigger-" + strings.Repeat("c", 32), Start: start, Messages: []Message{message}}
}
func TestSessionRepairSelectionBindsImmutableInputAndPreservesLegacy(t *testing.T) {
	in := repairExecutionFixture()
	run := strings.TrimPrefix(in.AcceptanceID, "trigger-")
	raw, err := in.Validate(run, "gaggle")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseExecutionInputs(raw, run, "gaggle")
	if err != nil || *parsed.Messages[0].RepairTarget != *in.Messages[0].RepairTarget {
		t.Fatal(parsed, err)
	}
	in.Messages[0].RepairTarget.ExpectedHeadSHA = strings.Repeat("d", 40)
	if _, err = in.Validate(run, "gaggle"); err == nil {
		t.Fatal("changed selected head retained authority")
	}
	in.Messages[0].RepairTarget = nil
	in.Start.MessageDigest = Digest([]byte(in.Messages[0].Text))
	legacy, err := in.Validate(run, "gaggle")
	if err != nil || strings.Contains(string(legacy), "repairTarget") {
		t.Fatal("legacy encoding changed", string(legacy), err)
	}
	request, _ := json.Marshal(MessageRequest{RequestID: "key", Text: "text"})
	if string(request) != `{"requestId":"key","text":"text"}` || MessageDigest("text", nil) != Digest([]byte("text")) {
		t.Fatal("legacy request/digest changed", string(request))
	}
	prior := in.Messages[0]
	prior.ActorKind, prior.Actor, prior.RepairTarget = "agent", nil, repairTargetFixture()
	in.Messages = append([]Message{prior}, in.Messages...)
	if _, err = in.Validate(run, "gaggle"); err == nil {
		t.Fatal("agent fabricated human repair selection")
	}
}
func TestSessionRepairSelectionClosedJSONAndIdentityBounds(t *testing.T) {
	target := repairTargetFixture()
	raw, err := MarshalPRRepairTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"text":"repair","repairTarget":` + string(raw) + `}`
	decoded, err := DecodeMessageContent([]byte(body))
	if err != nil || *decoded.RepairTarget != *target {
		t.Fatal(decoded, err)
	}
	for _, invalid := range []string{
		strings.Replace(body, `"text":"repair"`, `"text":"repair","text":"other"`, 1),
		strings.Replace(body, `"sourceId":"900"`, `"sourceId":"900","sourceId":"901"`, 1),
		strings.Replace(body, `"provider":"github"`, `"Provider":"github"`, 1),
		strings.Replace(body, `"provider":"github"`, `"provider":"github","token":"secret"`, 1),
		strings.Replace(body, `"expectedHeadSha"`, `"ExpectedHeadSha"`, 1),
		`{"text":"repair","repairTarget":null}`, `{"text":"repair","repairTarget":{}}`,
	} {
		if _, err := DecodeMessageContent([]byte(invalid)); err == nil {
			t.Fatal("ambiguous selection accepted", invalid)
		}
	}
	for _, alter := range []func(*PRRepairTarget){
		func(v *PRRepairTarget) { v.Repository.Owner = "https://foreign" },
		func(v *PRRepairTarget) { v.Repository.Name = "../repo" },
		func(v *PRRepairTarget) { v.Repository.Project = "unexpected" },
		func(v *PRRepairTarget) { v.ExpectedHeadSHA = strings.Repeat("a", 39) },
		func(v *PRRepairTarget) { v.SourceID = "01" },
		func(v *PRRepairTarget) { v.RepositorySourceID = strings.Repeat("1", 129) },
		func(v *PRRepairTarget) { v.SourceBindingID = strings.Repeat("a", 129) },
	} {
		copy := *target
		alter(&copy)
		if ValidatePRRepairTarget(&copy) == nil {
			t.Fatal("invalid native selection", copy)
		}
	}
	target.Repository = RepairRepository{Provider: "ado", Owner: "org", Project: "Project Space", Name: "repo"}
	target.RepositorySourceID = "12345678-1234-1234-1234-123456789abc"
	if ValidatePRRepairTarget(target) != nil {
		t.Fatal("ADO native identity refused")
	}
	if _, err := ParsePRRepairTarget(append(raw[:len(raw)-1], []byte(`,"id":"12"}`)...)); err == nil {
		t.Fatal("duplicate durable selection accepted")
	}
}
