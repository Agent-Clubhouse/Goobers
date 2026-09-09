package mutationsidecar

import (
	"bytes"
	"testing"
)

func TestRecoveryFactsFailClosedWithoutPartialReceipts(t *testing.T) {
	valid := []byte(`{"provider":"github","kind":"pull-request","id":"9","operation":"merge","runId":"owner"}`)
	for _, bad := range [][]byte{
		[]byte(`{`), []byte(`{"provider":"github","kind":"pull-request"}`),
		[]byte(`{"provider":"github","kind":"pull-request","id":"9","futureProof":true}`),
		append(append([]byte{}, valid...), []byte(` {}`)...),
		bytes.Repeat([]byte{'x'}, MaxBytes+1),
		bytes.Repeat([]byte{'\n'}, MaxLines+1),
	} {
		data := append(append(append([]byte{}, valid...), '\n'), bad...)
		facts, err := ParseRecoveryFacts(data)
		if err == nil || len(facts) != 0 {
			t.Fatalf("accepted incomplete recovery handoff: facts=%+v err=%v", facts, err)
		}
	}
	facts, err := ParseRecoveryFacts(append(valid, '\n'))
	if err != nil || len(facts) != 1 || facts[0].RunID != "owner" || facts[0].Operation != "merge" {
		t.Fatalf("lost receipt: %+v %v", facts, err)
	}
}
